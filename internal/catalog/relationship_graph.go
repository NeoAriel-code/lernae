package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"lernae/internal/domain"
	"lernae/internal/providers"
	"lernae/internal/providers/wikidata"
	"lernae/internal/universe"
)

const (
	RelationshipGraphMaxDepth    = 2
	RelationshipGraphMaxNodes    = 12
	RelationshipGraphMaxEdges    = 32
	RelationshipGraphMaxRequests = 8
	relationshipGraphCacheTTL    = 30 * 24 * time.Hour
)

var (
	ErrRelationshipResolutionUnavailable = errors.New("Universe relationship resolution is unavailable")
	ErrRelationshipAnchorNotFound        = errors.New("exact Universe relationship anchor was not found")
	ErrRelationshipAnchorAmbiguous       = errors.New("Universe relationship anchor is ambiguous")
	ErrRelationshipAnchorInvalid         = errors.New("invalid Universe relationship anchor")
)

// RelationshipMetadataSource is the small provider boundary needed for one
// explicit, bounded graph-resolution request.
type RelationshipMetadataSource interface {
	SearchEntities(ctx context.Context, query, language string, limit int) ([]wikidata.EntityCandidate, error)
	FetchEntity(ctx context.Context, qid string, languages []string) (providers.RelationshipMetadata, error)
}

type relationshipMetadataCache interface {
	Get(ctx context.Context, provider, externalID, parserVersion string, now time.Time) (providers.RelationshipMetadata, bool, error)
	Put(ctx context.Context, metadata providers.RelationshipMetadata, ttl time.Duration) error
}

// UniverseRelationshipResolver expands claims only after an explicit local
// Universe ID or exact local title/alias request. Search results are not an
// entry point to this resolver.
type UniverseRelationshipResolver struct {
	repository *SQLiteRepository
	source     RelationshipMetadataSource
	cache      relationshipMetadataCache
}

func NewUniverseRelationshipResolver(repository *SQLiteRepository, source RelationshipMetadataSource, cache *SQLiteRelationshipMetadataCache) *UniverseRelationshipResolver {
	resolver := &UniverseRelationshipResolver{repository: repository, source: source}
	if cache != nil {
		resolver.cache = cache
	}
	return resolver
}

func (application *UniverseApplication) ResolveRelationships(
	ctx context.Context, universeID domain.UniverseID, exactName, language string,
) (UniverseRelationshipResult, error) {
	if application == nil || application.relationshipResolver == nil {
		return UniverseRelationshipResult{}, ErrRelationshipResolutionUnavailable
	}
	if (universeID == "") == (strings.TrimSpace(exactName) == "") {
		return UniverseRelationshipResult{}, ErrRelationshipAnchorInvalid
	}
	if universeID != "" {
		return application.relationshipResolver.ResolveByUniverseID(ctx, universeID, language)
	}
	return application.relationshipResolver.ResolveByExactName(ctx, exactName, language)
}

type UniverseRelationshipResult struct {
	UniverseID       domain.UniverseID `json:"universe_id"`
	NodesVisited     int               `json:"nodes_visited"`
	EdgesVisited     int               `json:"edges_visited"`
	RequestsUsed     int               `json:"requests_used"`
	MaxDepth         int               `json:"max_depth"`
	AliasesAdded     int               `json:"aliases_added"`
	RelationsSet     int               `json:"relations_set"`
	SuggestionsAdded int               `json:"suggestions_added"`
	UnresolvedEdges  int               `json:"unresolved_edges"`
	BudgetLimited    bool              `json:"budget_limited"`
}

type relationshipAnchor struct {
	universeID domain.UniverseID
	query      string
	language   string
}

// ResolveByUniverseID resolves metadata for one caller-selected Universe.
func (resolver *UniverseRelationshipResolver) ResolveByUniverseID(ctx context.Context, id domain.UniverseID, language string) (UniverseRelationshipResult, error) {
	if resolver == nil || resolver.repository == nil {
		return UniverseRelationshipResult{}, ErrRelationshipResolutionUnavailable
	}
	if err := validateBoundedText("Universe ID", string(id), 128); err != nil {
		return UniverseRelationshipResult{}, ErrRelationshipAnchorInvalid
	}
	graph, err := resolver.repository.GetUniverse(ctx, id)
	if err != nil {
		if errors.Is(err, ErrUniverseNotFound) {
			return UniverseRelationshipResult{}, ErrRelationshipAnchorNotFound
		}
		return UniverseRelationshipResult{}, err
	}
	return resolver.resolve(ctx, relationshipAnchor{universeID: id, query: graph.Universe.Title, language: language})
}

// ResolveByExactName accepts only one exact normalized local Universe title or
// localized alias; substring or fuzzy matches never establish an anchor.
func (resolver *UniverseRelationshipResolver) ResolveByExactName(ctx context.Context, name, language string) (UniverseRelationshipResult, error) {
	if resolver == nil || resolver.repository == nil {
		return UniverseRelationshipResult{}, ErrRelationshipResolutionUnavailable
	}
	if err := validateBoundedText("Universe alias", name, 200); err != nil {
		return UniverseRelationshipResult{}, ErrRelationshipAnchorInvalid
	}
	if language != "" {
		normalizedLanguage, err := normalizeAliasLanguage(language)
		if err != nil {
			return UniverseRelationshipResult{}, ErrRelationshipAnchorInvalid
		}
		language = normalizedLanguage
	}
	matchedID, err := resolver.findExactUniverse(ctx, name, language)
	if err != nil {
		return UniverseRelationshipResult{}, err
	}
	return resolver.resolve(ctx, relationshipAnchor{universeID: matchedID, query: strings.TrimSpace(name), language: language})
}

func (resolver *UniverseRelationshipResolver) findExactUniverse(ctx context.Context, name, language string) (domain.UniverseID, error) {
	key := universe.NormalizeTitle(name)
	if key == "" {
		return "", ErrRelationshipAnchorInvalid
	}
	rows, err := resolver.repository.db.QueryContext(ctx, `SELECT id, title FROM universes ORDER BY id LIMIT ?`, maxUniverseResolveRows+1)
	if err != nil {
		return "", fmt.Errorf("load exact relationship anchor Universes: %w", err)
	}
	titles := make(map[domain.UniverseID]string)
	for rows.Next() {
		var id domain.UniverseID
		var title string
		if err := rows.Scan(&id, &title); err != nil {
			_ = rows.Close()
			return "", fmt.Errorf("decode exact relationship anchor Universe: %w", err)
		}
		titles[id] = title
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", fmt.Errorf("read exact relationship anchor Universes: %w", err)
	}
	_ = rows.Close()
	if len(titles) > maxUniverseResolveRows {
		return "", ErrUniverseResolveBoundsExceeded
	}
	matches := make(map[domain.UniverseID]struct{})
	for id, title := range titles {
		if universe.NormalizeTitle(title) == key {
			matches[id] = struct{}{}
		}
	}
	aliases, err := resolver.repository.db.QueryContext(ctx, `SELECT universe_id, alias, language_code FROM universe_aliases ORDER BY universe_id, alias LIMIT ?`, maxUniverseResolveAliases+1)
	if err != nil {
		return "", fmt.Errorf("load exact relationship anchor aliases: %w", err)
	}
	aliasCount := 0
	for aliases.Next() {
		var id domain.UniverseID
		var alias string
		var aliasLanguage sql.NullString
		if err := aliases.Scan(&id, &alias, &aliasLanguage); err != nil {
			_ = aliases.Close()
			return "", fmt.Errorf("decode exact relationship anchor alias: %w", err)
		}
		aliasCount++
		if _, exists := titles[id]; !exists || universe.NormalizeTitle(alias) != key {
			continue
		}
		if language != "" && (!aliasLanguage.Valid || !strings.EqualFold(aliasLanguage.String, language)) {
			continue
		}
		matches[id] = struct{}{}
	}
	if err := aliases.Err(); err != nil {
		_ = aliases.Close()
		return "", fmt.Errorf("read exact relationship anchor aliases: %w", err)
	}
	_ = aliases.Close()
	if aliasCount > maxUniverseResolveAliases {
		return "", ErrUniverseResolveBoundsExceeded
	}
	if len(matches) == 0 {
		return "", ErrRelationshipAnchorNotFound
	}
	if len(matches) > 1 {
		return "", ErrRelationshipAnchorAmbiguous
	}
	for id := range matches {
		return id, nil
	}
	return "", ErrRelationshipAnchorNotFound
}

func (resolver *UniverseRelationshipResolver) resolve(ctx context.Context, anchor relationshipAnchor) (UniverseRelationshipResult, error) {
	if resolver.source == nil {
		return UniverseRelationshipResult{}, ErrRelationshipResolutionUnavailable
	}
	language := anchor.language
	if language == "" {
		language = "en"
	}
	if err := ctx.Err(); err != nil {
		return UniverseRelationshipResult{}, err
	}
	anchorGraph, err := resolver.repository.GetUniverse(ctx, anchor.universeID)
	if err != nil {
		if errors.Is(err, ErrUniverseNotFound) {
			return UniverseRelationshipResult{}, ErrRelationshipAnchorNotFound
		}
		return UniverseRelationshipResult{}, err
	}
	searchTerm := anchor.query
	if searchTerm == "" {
		searchTerm = anchorGraph.Universe.Title
	}
	result := UniverseRelationshipResult{UniverseID: anchor.universeID}
	result.RequestsUsed++ // One candidate lookup is the maximum anchor-search cost.
	candidates, err := resolver.source.SearchEntities(ctx, searchTerm, language, wikidata.DefaultMaxSearchResults)
	if err != nil {
		return UniverseRelationshipResult{}, fmt.Errorf("search exact Wikidata anchor candidate: %w", err)
	}
	exactCandidates := make([]wikidata.EntityCandidate, 0, len(candidates))
	searchKey := universe.NormalizeTitle(searchTerm)
	for _, candidate := range candidates {
		if validCatalogWikidataEntityID(candidate.ID) && universe.NormalizeTitle(candidate.Label) == searchKey {
			exactCandidates = append(exactCandidates, candidate)
		}
	}
	sort.Slice(exactCandidates, func(i, j int) bool { return exactCandidates[i].ID < exactCandidates[j].ID })
	if len(exactCandidates) == 0 {
		return result, ErrRelationshipAnchorNotFound
	}
	if len(exactCandidates) != 1 {
		return result, ErrRelationshipAnchorAmbiguous
	}
	rootQID := exactCandidates[0].ID
	state := relationshipTraversal{resolver: resolver, ctx: ctx, language: language, result: result,
		universeID: anchor.universeID, anchorTitle: anchorGraph.Universe.Title, anchorQID: rootQID,
		entities: make(map[string]providers.RelationshipMetadata), workIDs: make(map[string]domain.WorkID),
		collections: make(map[string]string), aliases: make(map[string]domain.UniverseAlias),
		relations: make(map[string]domain.WorkRelation), collectionTargets: make(map[string]string),
		suggestions: make(map[domain.WorkID]struct{}),
	}
	if err := state.expand(rootQID); err != nil {
		return state.result, err
	}
	if err := state.apply(); err != nil {
		return state.result, err
	}
	return state.result, nil
}

type relationshipTraversal struct {
	resolver          *UniverseRelationshipResolver
	ctx               context.Context
	language          string
	result            UniverseRelationshipResult
	universeID        domain.UniverseID
	anchorTitle       string
	anchorQID         string
	entities          map[string]providers.RelationshipMetadata
	workIDs           map[string]domain.WorkID
	collections       map[string]string
	aliases           map[string]domain.UniverseAlias
	relations         map[string]domain.WorkRelation
	collectionTargets map[string]string
	suggestions       map[domain.WorkID]struct{}
	queue             []relationshipNode
	queued            map[string]struct{}
	processed         map[string]struct{}
}

type relationshipNode struct {
	qid   string
	depth int
}

func (state *relationshipTraversal) expand(rootQID string) error {
	state.queue = []relationshipNode{{qid: rootQID}}
	state.queued = map[string]struct{}{rootQID: {}}
	state.processed = make(map[string]struct{})
	for len(state.queue) > 0 {
		if err := state.ctx.Err(); err != nil {
			return err
		}
		node := state.queue[0]
		state.queue = state.queue[1:]
		if _, done := state.processed[node.qid]; done || node.depth > RelationshipGraphMaxDepth {
			continue
		}
		if state.result.NodesVisited >= RelationshipGraphMaxNodes {
			state.result.BudgetLimited = true
			break
		}
		metadata, err := state.metadata(node.qid)
		if errors.Is(err, errRelationshipGraphBudget) {
			state.result.BudgetLimited = true
			break
		}
		if err != nil {
			return err
		}
		state.processed[node.qid] = struct{}{}
		if node.depth > state.result.MaxDepth {
			state.result.MaxDepth = node.depth
		}
		if node.qid == state.anchorQID {
			state.planUniverseAliases(metadata)
		}
		sourceWorkID, sourceIsWork, err := state.localWorkID(metadata)
		if err != nil {
			return err
		}
		for _, claim := range metadata.Claims {
			if state.result.EdgesVisited >= RelationshipGraphMaxEdges {
				state.result.BudgetLimited = true
				return nil
			}
			state.result.EdgesVisited++
			switch claim.Property {
			case "P179":
				if !sourceIsWork {
					continue
				}
				target, err := state.metadata(claim.ObjectExternalID)
				if errors.Is(err, errRelationshipGraphBudget) {
					state.result.BudgetLimited = true
					return nil
				}
				if err != nil {
					return err
				}
				title := localizedCollectionTitle(target.Labels, state.language)
				if title == "" {
					continue
				}
				state.collections[target.ExternalID] = title
				collectionID := domain.WorkCollectionID(stableCatalogID("collection", "wikidata", target.ExternalID))
				version := metadata.ParserVersion
				relation := domain.WorkRelation{
					WorkID: sourceWorkID, Type: domain.WorkRelationPartOfSeries, TargetCollectionID: &collectionID,
					Ordinal: claim.Ordinal, Provenance: domain.WorkRelationProvenanceProvider, ProviderVersion: &version,
					Confidence: 0.95, Evidence: "wikidata:P179",
				}
				if claim.Ordinal != nil {
					relation.Evidence = "wikidata:P179+P1545"
				}
				state.planRelation(relation, target.ExternalID)
			case "P155", "P144":
				if !sourceIsWork {
					continue
				}
				if node.depth >= RelationshipGraphMaxDepth {
					if _, alreadyLoaded := state.entities[claim.ObjectExternalID]; !alreadyLoaded {
						state.result.BudgetLimited = true
						continue
					}
				}
				target, err := state.metadata(claim.ObjectExternalID)
				if errors.Is(err, errRelationshipGraphBudget) {
					state.result.BudgetLimited = true
					return nil
				}
				if err != nil {
					return err
				}
				targetWorkID, targetIsWork, err := state.localWorkID(target)
				if err != nil {
					return err
				}
				if !targetIsWork {
					continue
				}
				relationType := domain.WorkRelationFollows
				if claim.Property == "P144" {
					relationType = domain.WorkRelationAdaptationOf
				}
				version := metadata.ParserVersion
				targetID := targetWorkID
				relation := domain.WorkRelation{
					WorkID: sourceWorkID, Type: relationType, TargetWorkID: &targetID,
					Provenance: domain.WorkRelationProvenanceProvider, ProviderVersion: &version,
					Confidence: 0.95, Evidence: "wikidata:" + claim.Property,
				}
				state.planRelation(relation, "")
				state.enqueue(claim.ObjectExternalID, node.depth+1)
			case "P527":
				if node.depth >= RelationshipGraphMaxDepth {
					if _, alreadyLoaded := state.entities[claim.ObjectExternalID]; !alreadyLoaded {
						state.result.BudgetLimited = true
						continue
					}
				}
				target, err := state.metadata(claim.ObjectExternalID)
				if errors.Is(err, errRelationshipGraphBudget) {
					state.result.BudgetLimited = true
					return nil
				}
				if err != nil {
					return err
				}
				if targetWorkID, ok, err := state.localWorkID(target); err != nil {
					return err
				} else if ok {
					state.suggestions[targetWorkID] = struct{}{}
					state.enqueue(claim.ObjectExternalID, node.depth+1)
				}
			}
		}
	}
	return nil
}

var errRelationshipGraphBudget = errors.New("relationship graph request budget reached")

func (state *relationshipTraversal) metadata(qid string) (providers.RelationshipMetadata, error) {
	if metadata, ok := state.entities[qid]; ok {
		return metadata, nil
	}
	if !validCatalogWikidataEntityID(qid) {
		return providers.RelationshipMetadata{}, providers.ErrInvalidRelationshipMetadata
	}
	if state.result.NodesVisited >= RelationshipGraphMaxNodes {
		return providers.RelationshipMetadata{}, errRelationshipGraphBudget
	}
	if state.resolver.cache != nil {
		metadata, hit, err := state.resolver.cache.Get(state.ctx, "wikidata", qid, wikidata.ParserVersion, time.Now().UTC())
		if err != nil {
			return providers.RelationshipMetadata{}, fmt.Errorf("read parsed Wikidata relationship cache: %w", err)
		}
		if hit {
			if err := validateRelationshipEntity(metadata, qid); err != nil {
				return providers.RelationshipMetadata{}, err
			}
			state.entities[qid] = metadata
			state.result.NodesVisited++
			return metadata, nil
		}
	}
	if state.result.RequestsUsed >= RelationshipGraphMaxRequests {
		return providers.RelationshipMetadata{}, errRelationshipGraphBudget
	}
	state.result.RequestsUsed++
	metadata, err := state.resolver.source.FetchEntity(state.ctx, qid, relationshipLanguages(state.language))
	if err != nil {
		return providers.RelationshipMetadata{}, fmt.Errorf("fetch bounded Wikidata relationship entity: %w", err)
	}
	if err := validateRelationshipEntity(metadata, qid); err != nil {
		return providers.RelationshipMetadata{}, err
	}
	if state.resolver.cache != nil {
		if err := state.resolver.cache.Put(state.ctx, metadata, relationshipGraphCacheTTL); err != nil {
			return providers.RelationshipMetadata{}, fmt.Errorf("cache parsed Wikidata relationship metadata: %w", err)
		}
	}
	state.entities[qid] = metadata
	state.result.NodesVisited++
	return metadata, nil
}

func validateRelationshipEntity(metadata providers.RelationshipMetadata, qid string) error {
	if metadata.Provider != "wikidata" || metadata.ExternalID != qid || metadata.ParserVersion != wikidata.ParserVersion {
		return providers.ErrInvalidRelationshipMetadata
	}
	if err := metadata.Validate(); err != nil {
		return err
	}
	return nil
}

func relationshipLanguages(language string) []string {
	result := []string{language}
	if !strings.EqualFold(language, "en") {
		result = append(result, "en")
	}
	return result
}

func (state *relationshipTraversal) localWorkID(metadata providers.RelationshipMetadata) (domain.WorkID, bool, error) {
	if workID, ok := state.workIDs[metadata.ExternalID]; ok {
		return workID, true, nil
	}
	graph, err := state.resolver.repository.ResolveWikidataWork(state.ctx, metadata)
	if err != nil {
		if errors.Is(err, ErrWikidataWorkIdentityNotFound) || errors.Is(err, ErrWikidataWorkIdentityAmbiguous) {
			return "", false, nil
		}
		return "", false, err
	}
	state.workIDs[metadata.ExternalID] = graph.Work.ID
	return graph.Work.ID, true, nil
}

func (state *relationshipTraversal) planUniverseAliases(metadata providers.RelationshipMetadata) {
	values := append(append([]providers.RelationshipLocalizedValue(nil), metadata.Labels...), metadata.Aliases...)
	for _, value := range values {
		if universe.NormalizeTitle(value.Value) == universe.NormalizeTitle(state.anchorTitle) {
			continue
		}
		language := value.Language
		alias := domain.UniverseAlias{
			UniverseID: state.universeID, Alias: value.Value, Language: &language,
			Provenance:      string(domain.UniverseMembershipProvenanceProvider),
			ProviderVersion: relationshipTextPointer(metadata.EntityRevision), Confidence: 0.9,
		}
		key := relationshipAliasKey(alias)
		state.aliases[key] = alias
	}
}

func relationshipTextPointer(value string) *string { return &value }

func relationshipAliasKey(alias domain.UniverseAlias) string {
	language := ""
	if alias.Language != nil {
		language = strings.ToLower(*alias.Language)
	}
	return language + "\x00" + universe.NormalizeTitle(alias.Alias)
}

func localizedCollectionTitle(labels []providers.RelationshipLocalizedValue, requestedLanguage string) string {
	for _, language := range []string{requestedLanguage, "en"} {
		for _, label := range labels {
			if strings.EqualFold(label.Language, language) && strings.TrimSpace(label.Value) != "" {
				return label.Value
			}
		}
	}
	if len(labels) > 0 {
		return labels[0].Value
	}
	return ""
}

func (state *relationshipTraversal) planRelation(relation domain.WorkRelation, collectionQID string) {
	key := fmt.Sprintf("%s\x00%s\x00%s", relation.WorkID, relation.Type, relationTargetKey(relation))
	if prior, exists := state.relations[key]; exists && !sameOrdinal(prior.Ordinal, relation.Ordinal) {
		delete(state.relations, key) // Conflicting provider ordinals are ambiguous, not orderable.
		delete(state.collectionTargets, key)
		state.relations[key+"\x00ambiguous"] = domain.WorkRelation{}
		return
	}
	if _, ambiguous := state.relations[key+"\x00ambiguous"]; ambiguous {
		return
	}
	state.relations[key] = relation
	if collectionQID != "" {
		state.collectionTargets[key] = collectionQID
	}
}

func relationTargetKey(relation domain.WorkRelation) string {
	if relation.TargetWorkID != nil {
		return string(*relation.TargetWorkID)
	}
	if relation.TargetCollectionID != nil {
		return string(*relation.TargetCollectionID)
	}
	return ""
}

func sameOrdinal(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (state *relationshipTraversal) enqueue(qid string, depth int) {
	if depth > RelationshipGraphMaxDepth {
		return
	}
	if _, exists := state.queued[qid]; exists {
		return
	}
	state.queued[qid] = struct{}{}
	state.queue = append(state.queue, relationshipNode{qid: qid, depth: depth})
}

func (state *relationshipTraversal) apply() error {
	initial, err := state.resolver.repository.GetUniverse(state.ctx, state.universeID)
	if err != nil {
		return err
	}
	knownAliases := make(map[string]struct{}, len(initial.Aliases))
	for _, alias := range initial.Aliases {
		knownAliases[relationshipAliasKey(alias)] = struct{}{}
	}
	aliasKeys := sortedKeys(state.aliases)
	for _, key := range aliasKeys {
		alias := state.aliases[key]
		if _, exists := knownAliases[key]; !exists {
			state.result.AliasesAdded++
		}
		if _, err := state.resolver.repository.AddUniverseAlias(state.ctx, alias); err != nil {
			return err
		}
	}
	collectionIDs := make(map[string]domain.WorkCollectionID, len(state.collections))
	collectionQIDs := make([]string, 0, len(state.collections))
	for qid := range state.collections {
		collectionQIDs = append(collectionQIDs, qid)
	}
	sort.Strings(collectionQIDs)
	for _, qid := range collectionQIDs {
		collection, err := state.resolver.repository.GetWorkCollectionByExternalIdentity(state.ctx, "wikidata", qid)
		if errors.Is(err, ErrWorkCollectionNotFound) {
			generatedID := domain.WorkCollectionID(stableCatalogID("collection", "wikidata", qid))
			if _, collisionErr := state.resolver.repository.GetWorkCollection(state.ctx, generatedID); collisionErr == nil {
				// A pre-existing local ID without this exact QID mapping is not identity evidence.
				state.result.UnresolvedEdges++
				continue
			} else if !errors.Is(collisionErr, ErrWorkCollectionNotFound) {
				return collisionErr
			}
			collection, err = state.resolver.repository.CreateOrGetWorkCollectionByExternalIdentity(
				state.ctx, "wikidata", qid, state.collections[qid], domain.WorkCollectionTypeSeries)
		}
		if err != nil {
			return err
		}
		collectionIDs[qid] = collection.ID
	}
	relationKeys := sortedKeys(state.relations)
	for _, key := range relationKeys {
		relation := state.relations[key]
		if !relation.Valid() {
			continue
		}
		if relation.Type == domain.WorkRelationPartOfSeries {
			qid := state.collectionTargets[key]
			localID, exists := collectionIDs[qid]
			if !exists {
				continue
			}
			relation.TargetCollectionID = &localID
		}
		if _, err := state.resolver.repository.SetWorkRelation(state.ctx, relation); err != nil {
			return err
		}
		state.result.RelationsSet++
	}
	suggestionIDs := make([]domain.WorkID, 0, len(state.suggestions))
	for workID := range state.suggestions {
		suggestionIDs = append(suggestionIDs, workID)
	}
	sort.Slice(suggestionIDs, func(i, j int) bool { return suggestionIDs[i] < suggestionIDs[j] })
	for _, workID := range suggestionIDs {
		added, err := state.resolver.repository.addProviderUniverseSuggestion(state.ctx, state.universeID, workID)
		if err != nil {
			return err
		}
		if added {
			state.result.SuggestionsAdded++
		}
	}
	return nil
}

func (repository *SQLiteRepository) addProviderUniverseSuggestion(ctx context.Context, universeID domain.UniverseID, workID domain.WorkID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	now := time.Now().UTC()
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin provider Universe suggestion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	inserted, err := tx.ExecContext(ctx, `INSERT INTO universe_memberships
		(work_id, universe_id, provenance, confidence, evidence, reason, confirmed_by_user, accepted_at_utc, status)
		SELECT ?, ?, 'provider', 0.5, 'wikidata_P527', 'Wikidata P527 candidate requires review.', 0, ?, 'review_needed'
		WHERE 1 = 1
		AND NOT EXISTS (SELECT 1 FROM universe_exclusions WHERE work_id = ? AND universe_id = ?)
		AND NOT EXISTS (SELECT 1 FROM universe_memberships WHERE work_id = ?)
		ON CONFLICT(work_id) DO NOTHING`, workID, universeID, formatUniverseTime(now), workID, universeID, workID)
	if err != nil {
		return false, fmt.Errorf("persist provider Universe suggestion: %w", err)
	}
	count, err := inserted.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read provider Universe suggestion result: %w", err)
	}
	if count > 0 {
		if err := touchUniverseTx(ctx, tx, universeID, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit provider Universe suggestion: %w", err)
	}
	return count > 0, nil
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
