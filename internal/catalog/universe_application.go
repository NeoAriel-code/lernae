package catalog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"lernae/internal/domain"
	"lernae/internal/universe"
)

var (
	ErrUniverseOperationConflict     = errors.New("Universe operation ID was already used for another request")
	ErrUniverseDecisionNotFound      = errors.New("requested manual Universe decision was not found")
	ErrUniverseSearchBoundsExceeded  = errors.New("Universe search exceeded its bounded local catalog scan")
	ErrUniverseResolveBoundsExceeded = errors.New("Universe resolution exceeded its bounded local catalog scan")
)

const (
	maxUniverseSearchRows     = 5000
	maxUniverseSearchAliases  = 20000
	maxUniverseResolveRows    = 1000
	maxUniverseResolveAliases = 10000
	maxUniverseResolveResults = universe.MaxCandidateResults
	maxUniverseCreateWorks    = 20
	maxUniverseDetailItems    = 100
)

// UniverseApplication coordinates resolver reads and atomic manual decisions.
// It contains no provider client and does not accept raw provider payloads.
type UniverseApplication struct {
	repository           *SQLiteRepository
	relationshipResolver *UniverseRelationshipResolver
}

func NewUniverseApplication(repository *SQLiteRepository) *UniverseApplication {
	return &UniverseApplication{repository: repository}
}

func NewUniverseApplicationWithRelationshipResolver(repository *SQLiteRepository, resolver *UniverseRelationshipResolver) *UniverseApplication {
	return &UniverseApplication{repository: repository, relationshipResolver: resolver}
}

type UniverseSummary struct {
	ID                    domain.UniverseID
	Title                 string
	SortTitle             *string
	Description           *string
	Artwork               *string
	ExistenceConfidence   float64
	NamingConfidence      float64
	NamingConfidenceLevel domain.UniverseNamingConfidenceLevel
	NamingEvidence        domain.UniverseNamingEvidence
	Provenance            domain.UniverseProvenance
	ConfirmedByUser       bool
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type UniverseWorkRecord struct {
	WorkID          domain.WorkID
	Provider        string
	ExternalID      string
	Title           string
	Medium          domain.Medium
	WorkType        string
	Provenance      domain.UniverseMembershipProvenance
	Confidence      float64
	Evidence        string
	Reason          string
	ConfirmedByUser bool
}

type UniverseExclusionRecord struct {
	WorkID     domain.WorkID
	Provider   string
	ExternalID string
	Title      string
	Medium     domain.Medium
	WorkType   string
	Reason     string
}

type UniverseDetail struct {
	Universe             domain.Universe
	Aliases              []domain.UniverseAlias
	Memberships          []UniverseWorkRecord
	Suggestions          []UniverseWorkRecord
	Exclusions           []UniverseExclusionRecord
	MembershipsTruncated bool
	AliasesTruncated     bool
	SuggestionsTruncated bool
	ExclusionsTruncated  bool
}

type UniverseCreateResult struct {
	Detail  UniverseDetail
	Created bool
}

type UniverseMembershipResult struct {
	Detail  UniverseDetail
	Created bool
}

type UniverseResetAction string

const (
	UniverseResetClearExclusion    UniverseResetAction = "clear_exclusion"
	UniverseResetClearConfirmation UniverseResetAction = "clear_confirmation"
)

// SearchUniverses performs a bounded local-only title/alias search. Matching
// uses the same conservative Unicode title normalizer as proposal resolution.
func (application *UniverseApplication) SearchUniverses(ctx context.Context, query string, limit int) ([]UniverseSummary, error) {
	if err := validateBoundedText("Universe search query", query, 200); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 20 {
		return nil, errors.New("Universe search limit must be between 1 and 20")
	}
	normalizedQuery := universe.NormalizeTitle(query)
	if normalizedQuery == "" {
		return []UniverseSummary{}, nil
	}
	rows, err := application.repository.db.QueryContext(ctx, `SELECT id, title, sort_title, description, artwork, created_at_utc, updated_at_utc,
		existence_confidence, naming_confidence, naming_confidence_level, naming_evidence, provenance, confirmed_by_user
		FROM universes ORDER BY COALESCE(sort_title, title) COLLATE NOCASE, title COLLATE NOCASE, id LIMIT ?`, maxUniverseSearchRows+1)
	if err != nil {
		return nil, fmt.Errorf("search local Universes: %w", err)
	}
	defer rows.Close()
	var universes []UniverseSummary
	for rows.Next() {
		var item UniverseSummary
		var sortTitle, description, artwork sql.NullString
		var createdAt, updatedAt string
		if err := rows.Scan(&item.ID, &item.Title, &sortTitle, &description, &artwork, &createdAt, &updatedAt,
			&item.ExistenceConfidence, &item.NamingConfidence, &item.NamingConfidenceLevel, &item.NamingEvidence,
			&item.Provenance, &item.ConfirmedByUser); err != nil {
			return nil, fmt.Errorf("decode local Universe search result: %w", err)
		}
		item.SortTitle = nullStringPointer(sortTitle)
		item.Description = nullStringPointer(description)
		item.Artwork = nullStringPointer(artwork)
		item.CreatedAt, err = parseUniverseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("decode Universe creation timestamp in search: %w", err)
		}
		item.UpdatedAt, err = parseUniverseTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("decode Universe update timestamp in search: %w", err)
		}
		universes = append(universes, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read local Universe search results: %w", err)
	}
	if len(universes) > maxUniverseSearchRows {
		return nil, ErrUniverseSearchBoundsExceeded
	}
	rows.Close()

	aliases, err := application.repository.db.QueryContext(ctx, `SELECT universe_id, alias FROM universe_aliases ORDER BY universe_id, alias LIMIT ?`, maxUniverseSearchAliases+1)
	if err != nil {
		return nil, fmt.Errorf("search local Universe aliases: %w", err)
	}
	defer aliases.Close()
	aliasMatches := make(map[domain.UniverseID]bool)
	aliasCount := 0
	for aliases.Next() {
		var id domain.UniverseID
		var alias string
		if err := aliases.Scan(&id, &alias); err != nil {
			return nil, fmt.Errorf("decode local Universe alias search result: %w", err)
		}
		aliasCount++
		if strings.Contains(universe.NormalizeTitle(alias), normalizedQuery) {
			aliasMatches[id] = true
		}
	}
	if err := aliases.Err(); err != nil {
		return nil, fmt.Errorf("read local Universe aliases: %w", err)
	}
	if aliasCount > maxUniverseSearchAliases {
		return nil, ErrUniverseSearchBoundsExceeded
	}
	matched := make([]UniverseSummary, 0, limit)
	for _, item := range universes {
		if strings.Contains(universe.NormalizeTitle(item.Title), normalizedQuery) || aliasMatches[item.ID] {
			matched = append(matched, item)
			if len(matched) == limit {
				break
			}
		}
	}
	return matched, nil
}

func (application *UniverseApplication) GetUniverse(ctx context.Context, id domain.UniverseID) (UniverseDetail, error) {
	detail := UniverseDetail{
		Universe: domain.Universe{ID: id}, Aliases: []domain.UniverseAlias{},
		Memberships: []UniverseWorkRecord{}, Suggestions: []UniverseWorkRecord{}, Exclusions: []UniverseExclusionRecord{},
	}
	var sortTitle, description, artwork sql.NullString
	var createdAt, updatedAt string
	if err := application.repository.db.QueryRowContext(ctx, `SELECT title, sort_title, description, artwork, created_at_utc, updated_at_utc,
		existence_confidence, naming_confidence, naming_confidence_level, naming_evidence, provenance, confirmed_by_user FROM universes WHERE id = ?`, id).Scan(
		&detail.Universe.Title, &sortTitle, &description, &artwork, &createdAt, &updatedAt,
		&detail.Universe.ExistenceConfidence, &detail.Universe.NamingConfidence, &detail.Universe.NamingConfidenceLevel,
		&detail.Universe.NamingEvidence, &detail.Universe.Provenance, &detail.Universe.ConfirmedByUser); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UniverseDetail{}, fmt.Errorf("%w: %s", ErrUniverseNotFound, id)
		}
		return UniverseDetail{}, fmt.Errorf("load Universe detail: %w", err)
	}
	detail.Universe.SortTitle = nullStringPointer(sortTitle)
	detail.Universe.Description = nullStringPointer(description)
	detail.Universe.Artwork = nullStringPointer(artwork)
	var err error
	detail.Universe.CreatedAt, err = parseUniverseTime(createdAt)
	if err != nil {
		return UniverseDetail{}, fmt.Errorf("decode Universe creation timestamp: %w", err)
	}
	detail.Universe.UpdatedAt, err = parseUniverseTime(updatedAt)
	if err != nil {
		return UniverseDetail{}, fmt.Errorf("decode Universe update timestamp: %w", err)
	}
	aliasRows, err := application.repository.db.QueryContext(ctx, `SELECT alias, language_code, provenance,
		provider_version, confidence, confirmed_by_user, created_at_utc FROM universe_aliases
		WHERE universe_id = ? ORDER BY alias COLLATE BINARY, language_code COLLATE BINARY LIMIT ?`, id, maxUniverseDetailItems+1)
	if err != nil {
		return UniverseDetail{}, fmt.Errorf("load bounded Universe aliases: %w", err)
	}
	for aliasRows.Next() {
		var alias domain.UniverseAlias
		var language, providerVersion sql.NullString
		var aliasCreatedAt string
		alias.UniverseID = id
		if err := aliasRows.Scan(&alias.Alias, &language, &alias.Provenance, &providerVersion,
			&alias.Confidence, &alias.ConfirmedByUser, &aliasCreatedAt); err != nil {
			_ = aliasRows.Close()
			return UniverseDetail{}, fmt.Errorf("decode bounded Universe alias: %w", err)
		}
		alias.Language = nullStringPointer(language)
		alias.ProviderVersion = nullStringPointer(providerVersion)
		alias.CreatedAt, err = parseUniverseTime(aliasCreatedAt)
		if err != nil {
			_ = aliasRows.Close()
			return UniverseDetail{}, fmt.Errorf("decode Universe alias timestamp: %w", err)
		}
		detail.Aliases = append(detail.Aliases, alias)
	}
	if err := aliasRows.Err(); err != nil {
		_ = aliasRows.Close()
		return UniverseDetail{}, fmt.Errorf("read bounded Universe aliases: %w", err)
	}
	if err := aliasRows.Close(); err != nil {
		return UniverseDetail{}, fmt.Errorf("close bounded Universe aliases: %w", err)
	}
	if len(detail.Aliases) > maxUniverseDetailItems {
		detail.AliasesTruncated = true
		detail.Aliases = detail.Aliases[:maxUniverseDetailItems]
	}
	type membershipRow struct {
		workID     domain.WorkID
		provenance string
		confidence float64
		evidence   string
		reason     string
		confirmed  bool
	}
	loadMembershipRows := func(status domain.UniverseMembershipStatus) ([]membershipRow, bool, error) {
		rows, err := application.repository.db.QueryContext(ctx, `SELECT work_id, provenance, confidence, evidence, reason, confirmed_by_user
			FROM universe_memberships WHERE universe_id = ? AND status = ? ORDER BY work_id LIMIT ?`, id, status, maxUniverseDetailItems+1)
		if err != nil {
			return nil, false, fmt.Errorf("load bounded Universe %s records: %w", status, err)
		}
		var items []membershipRow
		for rows.Next() {
			var item membershipRow
			if err := rows.Scan(&item.workID, &item.provenance, &item.confidence, &item.evidence, &item.reason, &item.confirmed); err != nil {
				_ = rows.Close()
				return nil, false, fmt.Errorf("decode bounded Universe %s record: %w", status, err)
			}
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("read bounded Universe %s records: %w", status, err)
		}
		_ = rows.Close()
		truncated := len(items) > maxUniverseDetailItems
		if truncated {
			items = items[:maxUniverseDetailItems]
		}
		return items, truncated, nil
	}
	memberships, membershipsTruncated, err := loadMembershipRows(domain.UniverseMembershipStatusAccepted)
	if err != nil {
		return UniverseDetail{}, err
	}
	detail.MembershipsTruncated = membershipsTruncated
	suggestions, suggestionsTruncated, err := loadMembershipRows(domain.UniverseMembershipStatusReviewNeeded)
	if err != nil {
		return UniverseDetail{}, err
	}
	detail.SuggestionsTruncated = suggestionsTruncated
	appendRecord := func(membership membershipRow) (UniverseWorkRecord, error) {
		work, err := application.repository.universeWorkDisplay(ctx, membership.workID)
		if err != nil {
			return UniverseWorkRecord{}, err
		}
		record := UniverseWorkRecord{
			WorkID: membership.workID, Title: work.Title, Medium: work.Medium, WorkType: work.WorkType,
			Provenance: domain.UniverseMembershipProvenance(membership.provenance), Confidence: membership.confidence,
			Evidence: membership.evidence, Reason: membership.reason, ConfirmedByUser: membership.confirmed,
		}
		record.Provider, record.ExternalID = work.Provider, work.ExternalID
		return record, nil
	}
	for _, membership := range memberships {
		record, err := appendRecord(membership)
		if err != nil {
			return UniverseDetail{}, err
		}
		detail.Memberships = append(detail.Memberships, record)
	}
	for _, suggestion := range suggestions {
		record, err := appendRecord(suggestion)
		if err != nil {
			return UniverseDetail{}, err
		}
		detail.Suggestions = append(detail.Suggestions, record)
	}
	type exclusionRow struct {
		workID domain.WorkID
		reason string
	}
	exclusionRows, err := application.repository.db.QueryContext(ctx, `SELECT work_id, reason FROM universe_exclusions WHERE universe_id = ? ORDER BY work_id LIMIT ?`, id, maxUniverseDetailItems+1)
	if err != nil {
		return UniverseDetail{}, fmt.Errorf("load bounded Universe exclusions: %w", err)
	}
	var exclusions []exclusionRow
	for exclusionRows.Next() {
		var item exclusionRow
		if err := exclusionRows.Scan(&item.workID, &item.reason); err != nil {
			_ = exclusionRows.Close()
			return UniverseDetail{}, fmt.Errorf("decode bounded Universe exclusion: %w", err)
		}
		exclusions = append(exclusions, item)
	}
	if err := exclusionRows.Err(); err != nil {
		_ = exclusionRows.Close()
		return UniverseDetail{}, fmt.Errorf("read bounded Universe exclusions: %w", err)
	}
	_ = exclusionRows.Close()
	if len(exclusions) > maxUniverseDetailItems {
		detail.ExclusionsTruncated = true
		exclusions = exclusions[:maxUniverseDetailItems]
	}
	for _, exclusion := range exclusions {
		work, err := application.repository.universeWorkDisplay(ctx, exclusion.workID)
		if err != nil {
			return UniverseDetail{}, err
		}
		record := UniverseExclusionRecord{
			WorkID: exclusion.workID, Title: work.Title, Medium: work.Medium, WorkType: work.WorkType, Reason: exclusion.reason,
		}
		record.Provider, record.ExternalID = work.Provider, work.ExternalID
		detail.Exclusions = append(detail.Exclusions, record)
	}
	sort.Slice(detail.Memberships, func(i, j int) bool {
		if detail.Memberships[i].Provider != detail.Memberships[j].Provider {
			return detail.Memberships[i].Provider < detail.Memberships[j].Provider
		}
		return detail.Memberships[i].ExternalID < detail.Memberships[j].ExternalID
	})
	sort.Slice(detail.Exclusions, func(i, j int) bool {
		if detail.Exclusions[i].Provider != detail.Exclusions[j].Provider {
			return detail.Exclusions[i].Provider < detail.Exclusions[j].Provider
		}
		return detail.Exclusions[i].ExternalID < detail.Exclusions[j].ExternalID
	})
	return detail, nil
}

// GetWorkSeriesDetails attaches only already-persisted Series edges to an
// exact Work identity. It never resolves or writes provider data from Details.
func (application *UniverseApplication) GetWorkSeriesDetails(
	ctx context.Context, provider, externalID string,
) ([]domain.WorkSeriesDetails, bool, error) {
	return application.repository.GetWorkSeriesDetails(ctx, provider, externalID)
}

type universeWorkDisplay struct {
	Title      string
	Medium     domain.Medium
	WorkType   string
	Provider   string
	ExternalID string
}

func (repository *SQLiteRepository) universeWorkDisplay(ctx context.Context, id domain.WorkID) (universeWorkDisplay, error) {
	var display universeWorkDisplay
	err := repository.db.QueryRowContext(ctx, `SELECT w.title, w.medium, w.work_type,
		COALESCE((SELECT provider FROM external_identities WHERE work_id = w.id ORDER BY provider, external_id LIMIT 1), ''),
		COALESCE((SELECT external_id FROM external_identities WHERE work_id = w.id ORDER BY provider, external_id LIMIT 1), '')
		FROM works w WHERE w.id = ?`, id).Scan(&display.Title, &display.Medium, &display.WorkType, &display.Provider, &display.ExternalID)
	if errors.Is(err, sql.ErrNoRows) {
		return universeWorkDisplay{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return universeWorkDisplay{}, fmt.Errorf("load safe Work display for Universe: %w", err)
	}
	return display, nil
}

// Resolve produces proposals against bounded local state only. It neither
// materializes the supplied results nor accepts a membership.
func (application *UniverseApplication) Resolve(ctx context.Context, query string, results []domain.MetadataSearchResult) ([]universe.UniverseCandidate, error) {
	if err := validateBoundedText("Universe resolve query", query, 200); err != nil {
		return nil, err
	}
	if len(results) > maxUniverseResolveResults {
		return nil, errors.New("Universe resolution accepts at most 20 results")
	}
	for _, result := range results {
		if err := validateWorkMaterialization(domain.WorkMaterialization{
			Provider: result.Provider, ExternalID: result.ExternalID, Title: result.Title, Medium: result.Medium, WorkType: result.WorkType,
		}); err != nil {
			return nil, err
		}
	}
	universeRows, err := application.repository.db.QueryContext(ctx, `SELECT id, title, existence_confidence,
		naming_confidence, naming_confidence_level, naming_evidence, provenance, confirmed_by_user
		FROM universes ORDER BY id LIMIT ?`, maxUniverseResolveRows+1)
	if err != nil {
		return nil, fmt.Errorf("load Universe resolver catalog: %w", err)
	}
	var states []universe.UniverseState
	for universeRows.Next() {
		var state universe.UniverseState
		if err := universeRows.Scan(&state.Universe.ID, &state.Universe.Title, &state.Universe.ExistenceConfidence,
			&state.Universe.NamingConfidence, &state.Universe.NamingConfidenceLevel, &state.Universe.NamingEvidence,
			&state.Universe.Provenance, &state.Universe.ConfirmedByUser); err != nil {
			_ = universeRows.Close()
			return nil, fmt.Errorf("decode Universe resolver catalog: %w", err)
		}
		states = append(states, state)
	}
	if err := universeRows.Err(); err != nil {
		_ = universeRows.Close()
		return nil, fmt.Errorf("read Universe resolver catalog: %w", err)
	}
	_ = universeRows.Close()
	if len(states) > maxUniverseResolveRows {
		return nil, ErrUniverseResolveBoundsExceeded
	}
	stateByID := make(map[domain.UniverseID]int, len(states))
	for index := range states {
		stateByID[states[index].Universe.ID] = index
	}
	aliasRows, err := application.repository.db.QueryContext(ctx, `SELECT universe_id, alias, provenance, created_at_utc FROM universe_aliases ORDER BY universe_id, alias LIMIT ?`, maxUniverseResolveAliases+1)
	if err != nil {
		return nil, fmt.Errorf("load Universe resolver aliases: %w", err)
	}
	aliasCount := 0
	for aliasRows.Next() {
		var alias domain.UniverseAlias
		var createdAt string
		if err := aliasRows.Scan(&alias.UniverseID, &alias.Alias, &alias.Provenance, &createdAt); err != nil {
			_ = aliasRows.Close()
			return nil, fmt.Errorf("decode Universe resolver alias: %w", err)
		}
		aliasCount++
		if index, ok := stateByID[alias.UniverseID]; ok {
			states[index].Aliases = append(states[index].Aliases, alias)
		}
	}
	if err := aliasRows.Err(); err != nil {
		_ = aliasRows.Close()
		return nil, fmt.Errorf("read Universe resolver aliases: %w", err)
	}
	_ = aliasRows.Close()
	if aliasCount > maxUniverseResolveAliases {
		return nil, ErrUniverseResolveBoundsExceeded
	}
	workStates := make([]universe.WorkIdentityState, 0, len(results))
	for _, result := range results {
		state := universe.WorkIdentityState{Provider: result.Provider, ExternalID: result.ExternalID}
		var workID domain.WorkID
		err := application.repository.db.QueryRowContext(ctx, `SELECT work_id FROM external_identities WHERE provider = ? AND external_id = ?`, result.Provider, result.ExternalID).Scan(&workID)
		if err == nil {
			state.WorkID = workID
			var membership domain.UniverseMembership
			var provenance, acceptedAt string
			err = application.repository.db.QueryRowContext(ctx, `SELECT universe_id, provenance, confidence, evidence, reason, confirmed_by_user, accepted_at_utc FROM universe_memberships WHERE work_id = ? AND status = 'accepted'`, workID).
				Scan(&membership.UniverseID, &provenance, &membership.Confidence, &membership.Evidence, &membership.Reason, &membership.ConfirmedByUser, &acceptedAt)
			if err == nil {
				membership.WorkID = workID
				membership.Provenance = domain.UniverseMembershipProvenance(provenance)
				membership.AcceptedAt, err = parseUniverseTime(acceptedAt)
				if err != nil {
					return nil, fmt.Errorf("decode resolver membership timestamp: %w", err)
				}
				state.Membership = &membership
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("load resolver membership: %w", err)
			}
			exclusionRows, err := application.repository.db.QueryContext(ctx, `SELECT universe_id, reason, recorded_at_utc FROM universe_exclusions WHERE work_id = ? ORDER BY universe_id LIMIT ?`, workID, maxUniverseResolveRows+1)
			if err != nil {
				return nil, fmt.Errorf("load resolver exclusions: %w", err)
			}
			exclusionCount := 0
			for exclusionRows.Next() {
				var exclusion domain.UniverseExclusion
				var recordedAt string
				exclusion.WorkID = workID
				if err := exclusionRows.Scan(&exclusion.UniverseID, &exclusion.Reason, &recordedAt); err != nil {
					_ = exclusionRows.Close()
					return nil, fmt.Errorf("decode resolver exclusion: %w", err)
				}
				exclusion.RecordedAt, err = parseUniverseTime(recordedAt)
				if err != nil {
					_ = exclusionRows.Close()
					return nil, fmt.Errorf("decode resolver exclusion timestamp: %w", err)
				}
				exclusionCount++
				state.Exclusions = append(state.Exclusions, exclusion)
			}
			if err := exclusionRows.Err(); err != nil {
				_ = exclusionRows.Close()
				return nil, fmt.Errorf("read resolver exclusions: %w", err)
			}
			_ = exclusionRows.Close()
			if exclusionCount > maxUniverseResolveRows {
				return nil, ErrUniverseResolveBoundsExceeded
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("resolve exact Work identity for proposals: %w", err)
		}
		workStates = append(workStates, state)
	}
	return universe.Resolve(universe.ResolveInput{Query: query, Results: results, Universes: states, WorkStates: workStates}), nil
}

// CreateAndConfirm creates one Universe and all selected confirmed Work
// memberships in a single transaction. operationID is an opaque client token,
// not a title or content-derived identity.
func (application *UniverseApplication) CreateAndConfirm(ctx context.Context, operationID domain.UniverseOperationID, universe domain.Universe, works []domain.WorkMaterialization) (UniverseCreateResult, error) {
	if err := validateBoundedText("Universe operation ID", string(operationID), 128); err != nil {
		return UniverseCreateResult{}, err
	}
	var err error
	if universe, err = normalizeCreateUniverseMetadata(universe); err != nil {
		return UniverseCreateResult{}, err
	}
	canonicalWorks, err := canonicalCreateWorks(works)
	if err != nil {
		return UniverseCreateResult{}, err
	}
	digest, err := universeCreateDigest(universe, canonicalWorks)
	if err != nil {
		return UniverseCreateResult{}, err
	}
	tx, err := application.repository.db.BeginTx(ctx, nil)
	if err != nil {
		return UniverseCreateResult{}, fmt.Errorf("begin atomic Universe create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var priorDigest string
	var priorUniverseID domain.UniverseID
	err = tx.QueryRowContext(ctx, `SELECT request_digest, universe_id FROM universe_create_operations WHERE operation_id = ?`, operationID).Scan(&priorDigest, &priorUniverseID)
	if err == nil {
		if priorDigest != digest {
			return UniverseCreateResult{}, ErrUniverseOperationConflict
		}
		if err := tx.Commit(); err != nil {
			return UniverseCreateResult{}, fmt.Errorf("finish idempotent Universe create replay: %w", err)
		}
		detail, err := application.GetUniverse(ctx, priorUniverseID)
		if err != nil {
			return UniverseCreateResult{}, err
		}
		return UniverseCreateResult{Detail: detail, Created: false}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return UniverseCreateResult{}, fmt.Errorf("check Universe create operation: %w", err)
	}
	universeID, err := newLocalUniverseID()
	if err != nil {
		return UniverseCreateResult{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO universes
		(id, title, sort_title, description, artwork, created_at_utc, updated_at_utc, existence_confidence,
		naming_confidence, naming_confidence_level, naming_evidence, provenance, confirmed_by_user)
		VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?)`,
		universeID, universe.Title, universe.SortTitle, universe.Description, formatUniverseTime(now), formatUniverseTime(now),
		universe.ExistenceConfidence, universe.NamingConfidence, universe.NamingConfidenceLevel, universe.NamingEvidence,
		universe.Provenance, universe.ConfirmedByUser); err != nil {
		return UniverseCreateResult{}, fmt.Errorf("create Universe: %w", err)
	}
	for _, input := range canonicalWorks {
		workID, err := materializeExternalWorkTx(ctx, tx, input)
		if err != nil {
			return UniverseCreateResult{}, err
		}
		if _, err := setUniverseMembershipTx(ctx, tx, domain.UniverseMembership{
			UniverseID: universeID, WorkID: workID, Provenance: domain.UniverseMembershipProvenanceManual,
			Confidence: 1, Evidence: "user_confirmed_membership",
			Reason: "The user explicitly accepted this exact Work for the Universe.", ConfirmedByUser: true,
		}, now); err != nil {
			return UniverseCreateResult{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO universe_create_operations (operation_id, request_digest, universe_id, created_at_utc) VALUES (?, ?, ?, ?)`,
		operationID, digest, universeID, formatUniverseTime(now)); err != nil {
		return UniverseCreateResult{}, fmt.Errorf("record Universe create operation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return UniverseCreateResult{}, fmt.Errorf("commit atomic Universe create: %w", err)
	}
	detail, err := application.GetUniverse(ctx, universeID)
	if err != nil {
		return UniverseCreateResult{}, err
	}
	return UniverseCreateResult{Detail: detail, Created: true}, nil
}

func canonicalCreateWorks(works []domain.WorkMaterialization) ([]domain.WorkMaterialization, error) {
	if len(works) == 0 || len(works) > maxUniverseCreateWorks {
		return nil, errors.New("Universe create requires between 1 and 20 selected Works")
	}
	canonical := append([]domain.WorkMaterialization(nil), works...)
	seen := make(map[string]struct{}, len(canonical))
	for index := range canonical {
		if err := validateWorkMaterialization(canonical[index]); err != nil {
			return nil, err
		}
		key := canonical[index].Provider + "\x00" + canonical[index].ExternalID
		if _, ok := seen[key]; ok {
			return nil, errors.New("Universe create contains a duplicate exact provider identity")
		}
		seen[key] = struct{}{}
		canonical[index].Summary = normalizeWorkSummary(canonical[index].Summary)
	}
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Provider != canonical[j].Provider {
			return canonical[i].Provider < canonical[j].Provider
		}
		return canonical[i].ExternalID < canonical[j].ExternalID
	})
	return canonical, nil
}

func universeCreateDigest(universe domain.Universe, works []domain.WorkMaterialization) (string, error) {
	encoded, err := json.Marshal(struct {
		Title       string
		SortTitle   *string
		Description *string
		Works       []domain.WorkMaterialization
	}{Title: universe.Title, SortTitle: universe.SortTitle, Description: universe.Description, Works: works})
	if err != nil {
		return "", fmt.Errorf("encode Universe create digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func newLocalUniverseID() (domain.UniverseID, error) {
	var randomID [16]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return "", fmt.Errorf("generate local Universe ID: %w", err)
	}
	return domain.UniverseID("universe-" + hex.EncodeToString(randomID[:])), nil
}

// ConfirmUniverse records an explicit user confirmation of a Universe's
// existence without changing its provenance, confidence, or Work decisions.
func (application *UniverseApplication) ConfirmUniverse(ctx context.Context, universeID domain.UniverseID) (UniverseDetail, error) {
	if err := validateBoundedText("Universe ID", string(universeID), 128); err != nil {
		return UniverseDetail{}, err
	}
	tx, err := application.repository.db.BeginTx(ctx, nil)
	if err != nil {
		return UniverseDetail{}, fmt.Errorf("begin Universe confirmation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireUniverseTx(ctx, tx, universeID); err != nil {
		return UniverseDetail{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE universes SET confirmed_by_user = 1, updated_at_utc = ?
		WHERE id = ? AND confirmed_by_user = 0`, formatUniverseTime(now), universeID); err != nil {
		return UniverseDetail{}, fmt.Errorf("confirm Universe existence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return UniverseDetail{}, fmt.Errorf("commit Universe confirmation: %w", err)
	}
	return application.GetUniverse(ctx, universeID)
}

// ConfirmWork materializes one exact provider identity and confirms its
// membership atomically. Repeated requests reuse the same Work and relation.
func (application *UniverseApplication) ConfirmWork(ctx context.Context, universeID domain.UniverseID, input domain.WorkMaterialization) (UniverseMembershipResult, error) {
	if err := validateBoundedText("Universe ID", string(universeID), 128); err != nil {
		return UniverseMembershipResult{}, err
	}
	if err := validateWorkMaterialization(input); err != nil {
		return UniverseMembershipResult{}, err
	}
	tx, err := application.repository.db.BeginTx(ctx, nil)
	if err != nil {
		return UniverseMembershipResult{}, fmt.Errorf("begin exact Work confirmation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireUniverseTx(ctx, tx, universeID); err != nil {
		return UniverseMembershipResult{}, err
	}
	workID, err := materializeExternalWorkTx(ctx, tx, input)
	if err != nil {
		return UniverseMembershipResult{}, err
	}
	var existingCount int
	var existingUniverseID domain.UniverseID
	var existingProvenance, existingAcceptedAt string
	var existingEvidence, existingReason string
	var existingConfidence float64
	err = tx.QueryRowContext(ctx, `SELECT universe_id, provenance, confidence, evidence, reason, accepted_at_utc FROM universe_memberships WHERE work_id = ?`, workID).
		Scan(&existingUniverseID, &existingProvenance, &existingConfidence, &existingEvidence, &existingReason, &existingAcceptedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return UniverseMembershipResult{}, fmt.Errorf("load existing exact Work membership: %w", err)
	}
	membership := domain.UniverseMembership{
		UniverseID: universeID, WorkID: workID, Provenance: domain.UniverseMembershipProvenanceManual, Confidence: 1, ConfirmedByUser: true,
	}
	if err == nil {
		existingCount = 1
		if existingUniverseID == universeID {
			membership.Provenance = domain.UniverseMembershipProvenance(existingProvenance)
			membership.Confidence = existingConfidence
			membership.Evidence = existingEvidence
			membership.Reason = existingReason
			membership.AcceptedAt, err = parseUniverseTime(existingAcceptedAt)
			if err != nil {
				return UniverseMembershipResult{}, fmt.Errorf("decode existing exact Work membership timestamp: %w", err)
			}
		}
	}
	if _, err := setUniverseMembershipTx(ctx, tx, membership, time.Now().UTC()); err != nil {
		return UniverseMembershipResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return UniverseMembershipResult{}, fmt.Errorf("commit exact Work confirmation: %w", err)
	}
	detail, err := application.GetUniverse(ctx, universeID)
	if err != nil {
		return UniverseMembershipResult{}, err
	}
	return UniverseMembershipResult{Detail: detail, Created: existingCount == 0}, nil
}

func (application *UniverseApplication) RemoveWork(ctx context.Context, universeID domain.UniverseID, provider, externalID, reason string) (UniverseDetail, error) {
	if err := validateBoundedText("Universe ID", string(universeID), 128); err != nil {
		return UniverseDetail{}, err
	}
	if err := validateBoundedText("provider", provider, 128); err != nil {
		return UniverseDetail{}, err
	}
	if err := validateBoundedText("external ID", externalID, 512); err != nil {
		return UniverseDetail{}, err
	}
	if err := validateBoundedText("exclusion reason", reason, 512); err != nil {
		return UniverseDetail{}, err
	}
	if err := application.requireUniverse(ctx, universeID); err != nil {
		return UniverseDetail{}, err
	}
	graph, err := application.repository.GetWorkByExternalIdentity(ctx, provider, externalID)
	if err != nil {
		return UniverseDetail{}, err
	}
	if _, err := application.repository.RemoveUniverseMembership(ctx, graph.Work.ID, universeID, reason); err != nil {
		return UniverseDetail{}, err
	}
	return application.GetUniverse(ctx, universeID)
}

// RejectWork materializes an exact provider identity only as a Work, removes
// any target membership, and stores a separate durable exclusion atomically.
func (application *UniverseApplication) RejectWork(ctx context.Context, universeID domain.UniverseID, input domain.WorkMaterialization, reason string) (UniverseDetail, error) {
	if err := validateWorkMaterialization(input); err != nil {
		return UniverseDetail{}, err
	}
	if err := validateBoundedText("exclusion reason", reason, 512); err != nil {
		return UniverseDetail{}, err
	}
	tx, err := application.repository.db.BeginTx(ctx, nil)
	if err != nil {
		return UniverseDetail{}, fmt.Errorf("begin exact Work exclusion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireUniverseTx(ctx, tx, universeID); err != nil {
		return UniverseDetail{}, err
	}
	workID, err := materializeExternalWorkTx(ctx, tx, input)
	if err != nil {
		return UniverseDetail{}, err
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM universe_memberships WHERE work_id = ? AND universe_id = ?`, workID, universeID)
	if err != nil {
		return UniverseDetail{}, fmt.Errorf("remove rejected accepted membership: %w", err)
	}
	deletedCount, err := deleted.RowsAffected()
	if err != nil {
		return UniverseDetail{}, fmt.Errorf("read rejected membership result: %w", err)
	}
	var existingReason string
	existingErr := tx.QueryRowContext(ctx, `SELECT reason FROM universe_exclusions WHERE work_id = ? AND universe_id = ?`, workID, universeID).Scan(&existingReason)
	if existingErr != nil && !errors.Is(existingErr, sql.ErrNoRows) {
		return UniverseDetail{}, fmt.Errorf("load existing exact Work exclusion: %w", existingErr)
	}
	changed := deletedCount > 0 || errors.Is(existingErr, sql.ErrNoRows) || existingReason != reason
	if changed {
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `INSERT INTO universe_exclusions (work_id, universe_id, reason, recorded_at_utc) VALUES (?, ?, ?, ?)
			ON CONFLICT(work_id, universe_id) DO UPDATE SET reason = excluded.reason, recorded_at_utc = excluded.recorded_at_utc`,
			workID, universeID, reason, formatUniverseTime(now)); err != nil {
			return UniverseDetail{}, fmt.Errorf("record exact Work exclusion: %w", err)
		}
		if err := touchUniverseTx(ctx, tx, universeID, now); err != nil {
			return UniverseDetail{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return UniverseDetail{}, fmt.Errorf("commit exact Work exclusion: %w", err)
	}
	return application.GetUniverse(ctx, universeID)
}

// ResetDecision clears only the requested explicit decision. Clearing a
// manual-only membership deletes that row; clearing confirmation on a
// provider/rule membership preserves the underlying non-manual evidence.
func (application *UniverseApplication) ResetDecision(ctx context.Context, universeID domain.UniverseID, provider, externalID string, action UniverseResetAction) (UniverseDetail, error) {
	if err := validateBoundedText("Universe ID", string(universeID), 128); err != nil {
		return UniverseDetail{}, err
	}
	if err := validateBoundedText("provider", provider, 128); err != nil {
		return UniverseDetail{}, err
	}
	if err := validateBoundedText("external ID", externalID, 512); err != nil {
		return UniverseDetail{}, err
	}
	if action != UniverseResetClearExclusion && action != UniverseResetClearConfirmation {
		return UniverseDetail{}, errors.New("unsupported Universe reset action")
	}
	graph, err := application.repository.GetWorkByExternalIdentity(ctx, provider, externalID)
	if err != nil {
		return UniverseDetail{}, err
	}
	tx, err := application.repository.db.BeginTx(ctx, nil)
	if err != nil {
		return UniverseDetail{}, fmt.Errorf("begin Universe decision reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireUniverseTx(ctx, tx, universeID); err != nil {
		return UniverseDetail{}, err
	}
	if action == UniverseResetClearExclusion {
		result, err := tx.ExecContext(ctx, `DELETE FROM universe_exclusions WHERE work_id = ? AND universe_id = ?`, graph.Work.ID, universeID)
		if err != nil {
			return UniverseDetail{}, fmt.Errorf("clear exact Universe exclusion: %w", err)
		}
		if count, err := result.RowsAffected(); err != nil {
			return UniverseDetail{}, fmt.Errorf("read exclusion reset result: %w", err)
		} else if count == 0 {
			return UniverseDetail{}, ErrUniverseDecisionNotFound
		}
	} else {
		var provenance string
		var confirmed bool
		err := tx.QueryRowContext(ctx, `SELECT provenance, confirmed_by_user FROM universe_memberships WHERE work_id = ? AND universe_id = ?`, graph.Work.ID, universeID).
			Scan(&provenance, &confirmed)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !confirmed) {
			return UniverseDetail{}, ErrUniverseDecisionNotFound
		}
		if err != nil {
			return UniverseDetail{}, fmt.Errorf("load confirmed Universe decision: %w", err)
		}
		if domain.UniverseMembershipProvenance(provenance) == domain.UniverseMembershipProvenanceManual {
			if _, err := tx.ExecContext(ctx, `DELETE FROM universe_memberships WHERE work_id = ? AND universe_id = ?`, graph.Work.ID, universeID); err != nil {
				return UniverseDetail{}, fmt.Errorf("clear manual-only Universe membership: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE universe_memberships SET confirmed_by_user = 0,
			status = CASE WHEN provenance = 'automatic' AND (confidence < 0.85 OR evidence NOT IN (
				'exact_title_anchor', 'whole_phrase_anchor', 'explicit_alias', 'distinctive_whole_token'
			)) THEN 'review_needed' ELSE 'accepted' END WHERE work_id = ? AND universe_id = ?`, graph.Work.ID, universeID); err != nil {
			return UniverseDetail{}, fmt.Errorf("clear user confirmation from non-manual membership: %w", err)
		}
	}
	if err := touchUniverseTx(ctx, tx, universeID, time.Now().UTC()); err != nil {
		return UniverseDetail{}, err
	}
	if err := tx.Commit(); err != nil {
		return UniverseDetail{}, fmt.Errorf("commit Universe decision reset: %w", err)
	}
	return application.GetUniverse(ctx, universeID)
}

func requireUniverseTx(ctx context.Context, tx *sql.Tx, id domain.UniverseID) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM universes WHERE id = ?)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("check Universe before correction: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: %s", ErrUniverseNotFound, id)
	}
	return nil
}

func (application *UniverseApplication) requireUniverse(ctx context.Context, id domain.UniverseID) error {
	var exists bool
	if err := application.repository.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM universes WHERE id = ?)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("check Universe before correction: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: %s", ErrUniverseNotFound, id)
	}
	return nil
}

func materializeExternalWorkTx(ctx context.Context, tx *sql.Tx, input domain.WorkMaterialization) (domain.WorkID, error) {
	if err := validateWorkMaterialization(input); err != nil {
		return "", err
	}
	var existingID domain.WorkID
	err := tx.QueryRowContext(ctx, `SELECT work_id FROM external_identities WHERE provider = ? AND external_id = ?`, input.Provider, input.ExternalID).Scan(&existingID)
	if err == nil {
		return existingID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("check exact Work identity: %w", err)
	}
	workID, err := newLocalWorkID()
	if err != nil {
		return "", err
	}
	identityID := domain.ExternalIdentityID(stableCatalogID("identity", input.Provider, input.ExternalID))
	if _, err := tx.ExecContext(ctx, `INSERT INTO works (id, medium, work_type, title, summary) VALUES (?, ?, ?, ?, ?)`,
		workID, input.Medium, input.WorkType, input.Title, normalizeWorkSummary(input.Summary)); err != nil {
		return "", fmt.Errorf("create materialized Work: %w", err)
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO external_identities (id, work_id, provider, external_id) VALUES (?, ?, ?, ?) ON CONFLICT(provider, external_id) DO NOTHING`,
		identityID, workID, input.Provider, input.ExternalID)
	if err != nil {
		return "", fmt.Errorf("bind materialized Work identity: %w", err)
	}
	count, err := inserted.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("read materialized identity result: %w", err)
	}
	if count == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM works WHERE id = ?`, workID); err != nil {
			return "", fmt.Errorf("discard duplicate materialized Work: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT work_id FROM external_identities WHERE provider = ? AND external_id = ?`, input.Provider, input.ExternalID).Scan(&workID); err != nil {
			return "", fmt.Errorf("resolve existing exact Work identity: %w", err)
		}
	}
	return workID, nil
}

func validateWorkMaterialization(input domain.WorkMaterialization) error {
	if err := validateBoundedText("provider", input.Provider, 128); err != nil {
		return err
	}
	if err := validateBoundedText("external ID", input.ExternalID, 512); err != nil {
		return err
	}
	if err := validateBoundedText("Work title", input.Title, 512); err != nil {
		return err
	}
	if !input.Medium.Valid() {
		return fmt.Errorf("unsupported Work medium %q", input.Medium)
	}
	if err := validateBoundedText("Work type", input.WorkType, 128); err != nil {
		return err
	}
	return nil
}

func setUniverseMembershipTx(ctx context.Context, tx *sql.Tx, membership domain.UniverseMembership, now time.Time) (domain.UniverseMembership, error) {
	if err := validateBoundedText("Work ID", string(membership.WorkID), 128); err != nil {
		return domain.UniverseMembership{}, err
	}
	if err := validateBoundedText("Universe ID", string(membership.UniverseID), 128); err != nil {
		return domain.UniverseMembership{}, err
	}
	if membership.Evidence != "" {
		if err := validateBoundedText("Universe membership evidence", membership.Evidence, 64); err != nil {
			return domain.UniverseMembership{}, err
		}
	}
	if membership.Reason != "" {
		if err := validateBoundedText("Universe membership reason", membership.Reason, 512); err != nil {
			return domain.UniverseMembership{}, err
		}
	}
	if math.IsNaN(membership.Confidence) || math.IsInf(membership.Confidence, 0) || membership.Confidence < 0 || membership.Confidence > 1 {
		return domain.UniverseMembership{}, errors.New("Universe membership confidence must be between 0 and 1")
	}
	if membership.Status == "" {
		membership.Status = domain.UniverseMembershipStatusAccepted
	}
	if membership.Status != domain.UniverseMembershipStatusAccepted {
		return domain.UniverseMembership{}, fmt.Errorf("unsupported new Universe membership status %q", membership.Status)
	}
	acceptedAtOmitted := membership.AcceptedAt.IsZero()
	if acceptedAtOmitted {
		membership.AcceptedAt = now
	} else {
		membership.AcceptedAt = membership.AcceptedAt.UTC()
	}
	var current domain.UniverseMembership
	var currentProvenance, currentAcceptedAt, currentStatus string
	err := tx.QueryRowContext(ctx, `SELECT universe_id, provenance, confidence, evidence, reason, confirmed_by_user, accepted_at_utc, status FROM universe_memberships WHERE work_id = ?`, membership.WorkID).
		Scan(&current.UniverseID, &currentProvenance, &current.Confidence, &current.Evidence, &current.Reason, &current.ConfirmedByUser, &currentAcceptedAt, &currentStatus)
	hasCurrent := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.UniverseMembership{}, fmt.Errorf("load current Work membership: %w", err)
	}
	if hasCurrent {
		current.WorkID = membership.WorkID
		current.Provenance = domain.UniverseMembershipProvenance(currentProvenance)
		current.Status = domain.UniverseMembershipStatus(currentStatus)
		current.AcceptedAt, err = parseUniverseTime(currentAcceptedAt)
		if err != nil {
			return domain.UniverseMembership{}, fmt.Errorf("decode current membership timestamp: %w", err)
		}
		if current.ConfirmedByUser && !membership.ConfirmedByUser {
			return current, nil
		}
		if current.Status == domain.UniverseMembershipStatusReviewNeeded &&
			membership.Provenance == domain.UniverseMembershipProvenanceAutomatic && !membership.ConfirmedByUser {
			return current, nil
		}
		if current.UniverseID != membership.UniverseID && membership.Provenance == domain.UniverseMembershipProvenanceAutomatic {
			return current, nil
		}
		if current.UniverseID == membership.UniverseID {
			if acceptedAtOmitted {
				membership.AcceptedAt = current.AcceptedAt
			}
			if membership.Evidence == "" {
				membership.Evidence = current.Evidence
			}
			if membership.Reason == "" {
				membership.Reason = current.Reason
			}
		}
	}
	if membership.Provenance == domain.UniverseMembershipProvenanceAutomatic && !membership.ConfirmedByUser &&
		!supportedAutomaticMembership(membership.Confidence, membership.Evidence) {
		return domain.UniverseMembership{}, errors.New("automatic Universe membership requires high-confidence supported evidence")
	}
	// Migration 0008 preserves unknown provenance on legacy rows. Permit it only
	// when explicitly confirming that same persisted Work/Universe decision.
	legacyReconfirmation := !membership.Provenance.Valid() && hasCurrent && membership.ConfirmedByUser &&
		current.UniverseID == membership.UniverseID && current.Provenance == membership.Provenance
	if !membership.Provenance.Valid() && !legacyReconfirmation {
		return domain.UniverseMembership{}, fmt.Errorf("unsupported Universe membership provenance %q", membership.Provenance)
	}
	if membership.ConfirmedByUser && membership.Provenance != domain.UniverseMembershipProvenanceAutomatic && membership.Evidence == "" {
		membership.Evidence = string(universe.EvidenceUserConfirmedMembership)
		membership.Reason = "The user explicitly accepted this exact Work for the Universe."
	}
	var excluded bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM universe_exclusions WHERE work_id = ? AND universe_id = ?)`, membership.WorkID, membership.UniverseID).Scan(&excluded); err != nil {
		return domain.UniverseMembership{}, fmt.Errorf("check target membership exclusion: %w", err)
	}
	if excluded && !membership.ConfirmedByUser {
		return domain.UniverseMembership{}, fmt.Errorf("%w: Work %s, Universe %s", ErrUniverseMembershipExcluded, membership.WorkID, membership.UniverseID)
	}
	if hasCurrent && current.UniverseID == membership.UniverseID && current.Provenance == membership.Provenance &&
		current.Confidence == membership.Confidence && current.Evidence == membership.Evidence && current.Reason == membership.Reason &&
		current.ConfirmedByUser == membership.ConfirmedByUser && current.AcceptedAt.Equal(membership.AcceptedAt) &&
		current.Status == membership.Status && !excluded {
		return current, nil
	}
	if hasCurrent && membership.ConfirmedByUser && current.UniverseID != membership.UniverseID {
		if _, err := tx.ExecContext(ctx, `INSERT INTO universe_exclusions (work_id, universe_id, reason, recorded_at_utc) VALUES (?, ?, ?, ?)
			ON CONFLICT(work_id, universe_id) DO UPDATE SET reason = excluded.reason, recorded_at_utc = excluded.recorded_at_utc`,
			membership.WorkID, current.UniverseID, "reassigned through confirmed membership", formatUniverseTime(now)); err != nil {
			return domain.UniverseMembership{}, fmt.Errorf("record previous membership correction: %w", err)
		}
	}
	if membership.ConfirmedByUser {
		if _, err := tx.ExecContext(ctx, `DELETE FROM universe_exclusions WHERE work_id = ? AND universe_id = ?`, membership.WorkID, membership.UniverseID); err != nil {
			return domain.UniverseMembership{}, fmt.Errorf("clear exclusion for confirmed membership: %w", err)
		}
	}
	if legacyReconfirmation {
		// The v8 UPDATE trigger rejects any statement that rewrites provenance,
		// even when the value is unchanged, so update only confirmation fields.
		if _, err := tx.ExecContext(ctx, `UPDATE universe_memberships SET confidence = ?, evidence = ?, reason = ?,
			accepted_at_utc = ?, confirmed_by_user = ?, status = ? WHERE work_id = ? AND universe_id = ? AND provenance = ?`,
			membership.Confidence, membership.Evidence, membership.Reason, formatUniverseTime(membership.AcceptedAt),
			membership.ConfirmedByUser, membership.Status, membership.WorkID, membership.UniverseID, membership.Provenance); err != nil {
			return domain.UniverseMembership{}, fmt.Errorf("confirm legacy Universe membership without rewriting provenance: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `INSERT INTO universe_memberships
			(work_id, universe_id, provenance, confidence, evidence, reason, accepted_at_utc, confirmed_by_user, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(work_id) DO UPDATE SET universe_id = excluded.universe_id, provenance = excluded.provenance,
			confidence = excluded.confidence, evidence = excluded.evidence, reason = excluded.reason,
			accepted_at_utc = excluded.accepted_at_utc, confirmed_by_user = excluded.confirmed_by_user, status = excluded.status`,
			membership.WorkID, membership.UniverseID, membership.Provenance, membership.Confidence, membership.Evidence, membership.Reason,
			formatUniverseTime(membership.AcceptedAt), membership.ConfirmedByUser, membership.Status); err != nil {
			return domain.UniverseMembership{}, fmt.Errorf("save accepted Universe membership: %w", err)
		}
	}
	if hasCurrent && current.UniverseID != membership.UniverseID {
		if err := touchUniverseTx(ctx, tx, current.UniverseID, now); err != nil {
			return domain.UniverseMembership{}, err
		}
	}
	if err := touchUniverseTx(ctx, tx, membership.UniverseID, now); err != nil {
		return domain.UniverseMembership{}, err
	}
	return membership, nil
}

func supportedAutomaticMembership(confidence float64, evidence string) bool {
	if confidence < 0.85 {
		return false
	}
	switch universe.EvidenceType(evidence) {
	case universe.EvidenceExactTitleAnchor, universe.EvidenceWholePhraseAnchor,
		universe.EvidenceExplicitAlias, universe.EvidenceDistinctiveToken:
		return true
	default:
		return false
	}
}

func normalizeCreateUniverseMetadata(universe domain.Universe) (domain.Universe, error) {
	if err := validateBoundedText("Universe title", universe.Title, 512); err != nil {
		return domain.Universe{}, err
	}
	if universe.Artwork != nil {
		return domain.Universe{}, errors.New("Universe artwork is not accepted without a validated safe source")
	}
	if universe.Provenance == "" {
		universe.Provenance = domain.UniverseProvenanceManual
	}
	if !universe.Provenance.Valid() {
		return domain.Universe{}, fmt.Errorf("unsupported Universe provenance %q", universe.Provenance)
	}
	if universe.Provenance == domain.UniverseProvenanceManual {
		universe.ExistenceConfidence = 1
		universe.ConfirmedByUser = true
		universe.NamingConfidence = 1
		universe.NamingConfidenceLevel = domain.UniverseNamingConfidenceHigh
		universe.NamingEvidence = domain.UniverseNamingEvidenceUserAuthored
	} else if math.IsNaN(universe.ExistenceConfidence) || math.IsInf(universe.ExistenceConfidence, 0) ||
		universe.ExistenceConfidence <= 0 || universe.ExistenceConfidence > 1 || universe.ConfirmedByUser ||
		math.IsNaN(universe.NamingConfidence) || math.IsInf(universe.NamingConfidence, 0) ||
		universe.NamingConfidence <= 0 || universe.NamingConfidence > 1 ||
		(universe.NamingConfidenceLevel != domain.UniverseNamingConfidenceHigh && universe.NamingConfidenceLevel != domain.UniverseNamingConfidenceModerate) ||
		(universe.NamingEvidence != domain.UniverseNamingEvidenceDistinctiveQuery && universe.NamingEvidence != domain.UniverseNamingEvidenceSharedAnchor) {
		return domain.Universe{}, errors.New("automatic Universe discovery requires unconfirmed existence and safe naming confidence in (0, 1]")
	}
	var err error
	if universe.SortTitle, err = normalizeUniverseText("Universe sort title", universe.SortTitle, 512); err != nil {
		return domain.Universe{}, err
	}
	if universe.Description, err = normalizeUniverseText("Universe description", universe.Description, 2048); err != nil {
		return domain.Universe{}, err
	}
	return universe, nil
}
