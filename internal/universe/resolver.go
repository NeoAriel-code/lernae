// Package universe contains pure Universe proposal and matching rules.
// It intentionally has no repository or provider dependencies: callers pass
// normalized Search results and catalog snapshots, and the resolver never
// persists or accepts a relationship.
package universe

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"lernae/internal/domain"
)

// EvidenceType identifies the bounded rule that produced a membership
// proposal. These values are descriptive categories, not a combined score.
type EvidenceType string

const (
	EvidenceConfirmedIdentity       EvidenceType = "confirmed_identity"
	EvidenceUserConfirmedMembership EvidenceType = "user_confirmed_membership"
	EvidenceExistingMembership      EvidenceType = "existing_membership"
	EvidenceExactTitleAnchor        EvidenceType = "exact_title_anchor"
	EvidenceWholePhraseAnchor       EvidenceType = "whole_phrase_anchor"
	EvidenceExplicitAlias           EvidenceType = "explicit_alias"
	EvidenceDistinctiveToken        EvidenceType = "distinctive_whole_token"
	EvidenceExplicitExclusion       EvidenceType = "explicit_exclusion"
)

// ConfidenceLevel is a rule-strength category. Confidence values are fixed by
// evidence type and are never accumulated or inferred from provider scores.
type ConfidenceLevel string

const (
	MaxCandidateResults                   = 20
	MaxUniverseCandidates                 = 20
	ConfidenceNone        ConfidenceLevel = "none"
	ConfidenceModerate    ConfidenceLevel = "moderate"
	ConfidenceHigh        ConfidenceLevel = "high"
)

// MembershipState describes the exact Work/Universe pair before this
// in-memory proposal was produced.
type MembershipState string

const (
	MembershipNone              MembershipState = "none"
	MembershipAccepted          MembershipState = "accepted"
	MembershipAcceptedElsewhere MembershipState = "accepted_elsewhere"
	MembershipExcluded          MembershipState = "excluded"
)

// UniverseState is the resolver's read-only snapshot of a Universe and its
// explicitly stored aliases.
type UniverseState struct {
	Universe domain.Universe
	Aliases  []domain.UniverseAlias
}

// WorkIdentityState connects an exact provider identity to its current
// accepted relation and explicit exclusions. It is supplied by the caller;
// resolver logic does not look up or mutate persistence.
type WorkIdentityState struct {
	Provider   string
	ExternalID string
	WorkID     domain.WorkID
	Membership *domain.UniverseMembership
	Exclusions []domain.UniverseExclusion
}

// ResolveInput is a deterministic, provider-neutral resolver request.
type ResolveInput struct {
	Query      string
	Results    []domain.MetadataSearchResult
	Universes  []UniverseState
	WorkStates []WorkIdentityState
}

// UniverseCandidate is a possible existing or not-yet-created Universe. An
// empty UniverseID means the candidate is anchored only to the query; it is
// still only a proposal and has no persisted identity.
type UniverseCandidate struct {
	UniverseID            domain.UniverseID
	Title                 string
	Existing              bool
	ExistenceConfidence   float64
	NamingConfidence      float64
	NamingConfidenceLevel ConfidenceLevel
	NamingEvidence        domain.UniverseNamingEvidence
	Evidence              EvidenceType
	Reason                string
	Confidence            float64
	ConfidenceLevel       ConfidenceLevel
	Memberships           []MembershipProposal
}

// MembershipProposal carries the exact provider identity and safe display
// fields for one hit. Suppressed exclusions are returned as non-actionable
// diagnostics so callers can explain why no membership was proposed.
type MembershipProposal struct {
	UniverseID                   domain.UniverseID
	UniverseTitle                string
	Provider                     string
	ExternalID                   string
	MediaType                    string
	Title                        string
	WorkID                       domain.WorkID
	ExistingMembershipUniverseID domain.UniverseID
	ExistingState                MembershipState
	ConfirmedByUser              bool
	Evidence                     EvidenceType
	Reason                       string
	Confidence                   float64
	ConfidenceLevel              ConfidenceLevel
	Suppressed                   bool
	RequiresConfirmation         bool
}

type identityKey struct {
	provider   string
	externalID string
}

type identitySnapshot struct {
	state     WorkIdentityState
	ambiguous bool
}

type candidateKey struct {
	universeID domain.UniverseID
	queryGroup bool
	provider   string
	externalID string
}

type candidateAccumulator struct {
	candidate UniverseCandidate
	members   map[identityKey]MembershipProposal
}

// NormalizeTitle applies Unicode compatibility normalization and full case
// folding, then treats punctuation and whitespace as token boundaries. It
// preserves letters, numbers, and combining marks; accents are not erased.
func NormalizeTitle(value string) string {
	value = norm.NFKC.String(cases.Fold().String(norm.NFKC.String(value)))
	var normalized strings.Builder
	normalized.Grow(len(value))
	separatorPending := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) {
			if separatorPending && normalized.Len() > 0 {
				normalized.WriteByte(' ')
			}
			normalized.WriteRune(r)
			separatorPending = false
			continue
		}
		separatorPending = normalized.Len() > 0
	}
	return normalized.String()
}

// Resolve returns deterministic, explainable candidates without persistence
// or automatic membership acceptance. Identity is exact provider+external ID;
// display titles are never used to deduplicate provider identities.
func Resolve(input ResolveInput) []UniverseCandidate {
	if len(input.Results) > MaxCandidateResults {
		return []UniverseCandidate{}
	}
	query := NormalizeTitle(input.Query)
	if query == "" {
		return []UniverseCandidate{}
	}

	universeStates := canonicalUniverseStates(input.Universes)
	workStates := identityStates(input.WorkStates)
	results := canonicalResults(input.Results)
	queryTokens := tokens(query)
	distinctiveQueryTokens := distinctiveTokens(queryTokens)
	allowSharedExactAnchor := len(distinctiveQueryTokens) > 0 || exactIdentityCount(query, results) <= 1

	accumulators := make(map[candidateKey]*candidateAccumulator)
	for _, result := range results {
		key := identityKey{provider: result.Provider, externalID: result.ExternalID}
		snapshot, hasWorkState := workStates[key]
		if hasWorkState && snapshot.ambiguous {
			continue
		}
		workState := snapshot.state
		membership := (*domain.UniverseMembership)(nil)
		if hasWorkState {
			membership = workState.Membership
		}

		matchedExisting := false
		confirmedElsewhere := membership != nil && membership.ConfirmedByUser
		for _, state := range universeStates {
			universeID := state.Universe.ID
			if confirmedElsewhere && membership.UniverseID != universeID {
				continue
			}

			if membership != nil && membership.UniverseID == universeID {
				proposal := proposalFor(result, state, membership, EvidenceExistingMembership)
				if membership.ConfirmedByUser && proposal.Evidence == EvidenceExistingMembership {
					proposal.Evidence = EvidenceConfirmedIdentity
					proposal.Reason = reasonFor(EvidenceConfirmedIdentity)
					proposal.Confidence, proposal.ConfidenceLevel = confidenceFor(EvidenceConfirmedIdentity)
				}
				if exclusionFor(workState, universeID) {
					proposal = suppressedProposal(result, state, workState, membership)
				}
				addProposal(accumulators, candidateKey{universeID: universeID}, state.Universe, true, proposal)
				matchedExisting = true
				continue
			}

			evidence, matched := matchUniverse(query, distinctiveQueryTokens, result.Title, state, allowSharedExactAnchor)
			if !matched {
				continue
			}
			matchedExisting = true
			if hasWorkState && exclusionFor(workState, universeID) {
				addProposal(accumulators, candidateKey{universeID: universeID}, state.Universe, true,
					suppressedProposal(result, state, workState, membership))
				continue
			}

			proposal := proposalFor(result, state, membership, evidence)
			addProposal(accumulators, candidateKey{universeID: universeID}, state.Universe, true, proposal)
		}

		// A confirmed exact identity must remain attached to its accepted
		// Universe even when the current query points somewhere else.
		if confirmedElsewhere || matchedExisting {
			continue
		}

		if len(distinctiveQueryTokens) > 0 {
			if NormalizeTitle(result.Title) == query {
				proposal := proposalFor(result, UniverseState{}, membership, EvidenceExactTitleAnchor)
				addProposal(accumulators, candidateKey{queryGroup: true}, domain.Universe{Title: input.Query}, false, proposal)
				continue
			}
			if hasDistinctiveWholeToken(distinctiveQueryTokens, tokens(NormalizeTitle(result.Title))) {
				proposal := proposalFor(result, UniverseState{}, membership, EvidenceDistinctiveToken)
				addProposal(accumulators, candidateKey{queryGroup: true}, domain.Universe{Title: input.Query}, false, proposal)
			}
			continue
		}

		// An exact query/title anchor is still useful for a short or common
		// phrase, but each provider identity stays a separate candidate.
		if NormalizeTitle(result.Title) == query {
			proposal := proposalFor(result, UniverseState{}, membership, EvidenceExactTitleAnchor)
			addProposal(accumulators, candidateKey{provider: key.provider, externalID: key.externalID},
				domain.Universe{Title: input.Query}, false, proposal)
		}
	}

	return finalizeCandidates(accumulators)
}

func canonicalUniverseStates(states []UniverseState) []UniverseState {
	states = append([]UniverseState(nil), states...)
	sort.Slice(states, func(i, j int) bool {
		if states[i].Universe.ID != states[j].Universe.ID {
			return states[i].Universe.ID < states[j].Universe.ID
		}
		return NormalizeTitle(states[i].Universe.Title) < NormalizeTitle(states[j].Universe.Title)
	})
	seen := make(map[domain.UniverseID]struct{}, len(states))
	canonical := make([]UniverseState, 0, len(states))
	for _, state := range states {
		if state.Universe.ID == "" || strings.TrimSpace(state.Universe.Title) == "" {
			continue
		}
		if _, exists := seen[state.Universe.ID]; exists {
			continue
		}
		seen[state.Universe.ID] = struct{}{}
		aliases := make([]domain.UniverseAlias, 0, len(state.Aliases))
		for _, alias := range state.Aliases {
			if alias.UniverseID == state.Universe.ID {
				aliases = append(aliases, alias)
			}
		}
		state.Aliases = aliases
		sort.Slice(state.Aliases, func(i, j int) bool {
			return NormalizeTitle(state.Aliases[i].Alias) < NormalizeTitle(state.Aliases[j].Alias)
		})
		canonical = append(canonical, state)
	}
	return canonical
}

func identityStates(states []WorkIdentityState) map[identityKey]identitySnapshot {
	byIdentity := make(map[identityKey]identitySnapshot, len(states))
	for _, state := range states {
		if strings.TrimSpace(state.Provider) == "" || strings.TrimSpace(state.ExternalID) == "" {
			continue
		}
		key := identityKey{provider: state.Provider, externalID: state.ExternalID}
		ambiguous := false
		if state.Membership != nil {
			if state.WorkID == "" {
				state.WorkID = state.Membership.WorkID
			}
			if state.WorkID == "" || state.Membership.WorkID != state.WorkID {
				ambiguous = true
			}
		}
		for _, exclusion := range state.Exclusions {
			if state.WorkID == "" || exclusion.WorkID != state.WorkID {
				ambiguous = true
			}
		}
		if snapshot, found := byIdentity[key]; found {
			snapshot.ambiguous = true
			snapshot.state = WorkIdentityState{}
			byIdentity[key] = snapshot
			continue
		}
		if ambiguous {
			byIdentity[key] = identitySnapshot{ambiguous: true}
			continue
		}
		byIdentity[key] = identitySnapshot{state: state}
	}
	return byIdentity
}

func canonicalResults(results []domain.MetadataSearchResult) []domain.MetadataSearchResult {
	results = append([]domain.MetadataSearchResult(nil), results...)
	valid := results[:0]
	for _, result := range results {
		if strings.TrimSpace(result.Provider) == "" || strings.TrimSpace(result.ExternalID) == "" || strings.TrimSpace(result.Title) == "" {
			continue
		}
		valid = append(valid, result)
	}
	sort.Slice(valid, func(i, j int) bool {
		if valid[i].Provider != valid[j].Provider {
			return valid[i].Provider < valid[j].Provider
		}
		if valid[i].ExternalID != valid[j].ExternalID {
			return valid[i].ExternalID < valid[j].ExternalID
		}
		if NormalizeTitle(valid[i].Title) != NormalizeTitle(valid[j].Title) {
			return NormalizeTitle(valid[i].Title) < NormalizeTitle(valid[j].Title)
		}
		if valid[i].MediaType != valid[j].MediaType {
			return valid[i].MediaType < valid[j].MediaType
		}
		return valid[i].Title < valid[j].Title
	})
	return valid
}

func matchUniverse(query string, distinctiveQueryTokens []string, title string, state UniverseState, allowExactTitleAnchor bool) (EvidenceType, bool) {
	resultTitle := NormalizeTitle(title)
	if allowExactTitleAnchor && query == resultTitle && query == NormalizeTitle(state.Universe.Title) {
		return EvidenceExactTitleAnchor, true
	}

	for _, alias := range state.Aliases {
		aliasTitle := NormalizeTitle(alias.Alias)
		if aliasTitle == "" || query != aliasTitle {
			continue
		}
		if containsTokenSequence(tokens(resultTitle), tokens(aliasTitle)) {
			return EvidenceExplicitAlias, true
		}
	}

	if len(distinctiveQueryTokens) == 0 {
		return "", false
	}
	anchors := append([]string{NormalizeTitle(state.Universe.Title)}, aliasTitles(state.Aliases)...)
	resultTokens := tokens(resultTitle)
	for _, anchor := range anchors {
		anchorTokens := tokens(anchor)
		if hasDistinctiveWholeToken(distinctiveQueryTokens, anchorTokens) &&
			hasDistinctiveWholeToken(distinctiveQueryTokens, resultTokens) &&
			sharesDistinctiveToken(anchorTokens, resultTokens, distinctiveQueryTokens) {
			return EvidenceDistinctiveToken, true
		}
	}
	return "", false
}

func exactIdentityCount(query string, results []domain.MetadataSearchResult) int {
	identities := make(map[identityKey]struct{})
	for _, result := range results {
		if NormalizeTitle(result.Title) == query {
			identities[identityKey{provider: result.Provider, externalID: result.ExternalID}] = struct{}{}
		}
	}
	return len(identities)
}

func aliasTitles(aliases []domain.UniverseAlias) []string {
	titles := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		titles = append(titles, NormalizeTitle(alias.Alias))
	}
	return titles
}

func proposalFor(result domain.MetadataSearchResult, state UniverseState, membership *domain.UniverseMembership, evidence EvidenceType) MembershipProposal {
	confidence, level := confidenceFor(evidence)
	proposal := MembershipProposal{
		UniverseID: state.Universe.ID, UniverseTitle: state.Universe.Title,
		Provider: result.Provider, ExternalID: result.ExternalID, MediaType: result.MediaType, Title: result.Title,
		Evidence: evidence, Reason: reasonFor(evidence), Confidence: confidence, ConfidenceLevel: level,
		ExistingState: MembershipNone, RequiresConfirmation: true,
	}
	if membership == nil {
		return proposal
	}
	if storedEvidence := EvidenceType(membership.Evidence); validEvidenceType(storedEvidence) {
		proposal.Evidence = storedEvidence
		proposal.Reason = membership.Reason
		if proposal.Reason == "" {
			proposal.Reason = reasonFor(storedEvidence)
		}
		proposal.Confidence, proposal.ConfidenceLevel = confidenceFor(storedEvidence)
		if membership.Confidence > 0 {
			proposal.Confidence = membership.Confidence
		}
	}
	proposal.WorkID = membership.WorkID
	proposal.ExistingMembershipUniverseID = membership.UniverseID
	proposal.ConfirmedByUser = membership.ConfirmedByUser
	if membership.UniverseID == state.Universe.ID {
		proposal.ExistingState = MembershipAccepted
		proposal.RequiresConfirmation = false
	} else {
		proposal.ExistingState = MembershipAcceptedElsewhere
	}
	return proposal
}

func suppressedProposal(result domain.MetadataSearchResult, state UniverseState, workState WorkIdentityState, membership *domain.UniverseMembership) MembershipProposal {
	proposal := proposalFor(result, state, membership, EvidenceExplicitExclusion)
	proposal.ExistingState = MembershipExcluded
	proposal.Suppressed = true
	proposal.RequiresConfirmation = false
	proposal.WorkID = workState.WorkID
	proposal.Confidence, proposal.ConfidenceLevel = confidenceFor(EvidenceExplicitExclusion)
	proposal.Reason = reasonFor(EvidenceExplicitExclusion)
	return proposal
}

func exclusionFor(state WorkIdentityState, universeID domain.UniverseID) bool {
	for _, exclusion := range state.Exclusions {
		if state.WorkID != "" && exclusion.WorkID == state.WorkID && exclusion.UniverseID == universeID {
			return true
		}
	}
	return false
}

func addProposal(accumulators map[candidateKey]*candidateAccumulator, key candidateKey, universe domain.Universe, existing bool, proposal MembershipProposal) {
	accumulator := accumulators[key]
	if accumulator == nil {
		namingConfidence := universe.NamingConfidence
		namingLevel := confidenceLevelFromDomain(universe.NamingConfidenceLevel)
		namingEvidence := universe.NamingEvidence
		title := universe.Title
		if !existing {
			if safeTitle, confidence, level, evidence, ok := safeDiscoveryName(NormalizeTitle(universe.Title), nil); ok {
				title, namingConfidence, namingLevel, namingEvidence = safeTitle, confidence, level, evidence
			}
		}
		accumulator = &candidateAccumulator{
			candidate: UniverseCandidate{
				UniverseID: universe.ID, Title: title, Existing: existing,
				ExistenceConfidence: universe.ExistenceConfidence,
				NamingConfidence:    namingConfidence, NamingConfidenceLevel: namingLevel, NamingEvidence: namingEvidence,
			},
			members: make(map[identityKey]MembershipProposal),
		}
		accumulators[key] = accumulator
	}
	identity := identityKey{provider: proposal.Provider, externalID: proposal.ExternalID}
	if previous, found := accumulator.members[identity]; !found || preferProposal(proposal, previous) {
		accumulator.members[identity] = proposal
	}
	if proposal.Suppressed {
		if accumulator.candidate.Evidence == "" {
			accumulator.candidate.Evidence = EvidenceExplicitExclusion
			accumulator.candidate.Reason = proposal.Reason
			accumulator.candidate.Confidence, accumulator.candidate.ConfidenceLevel = confidenceFor(EvidenceExplicitExclusion)
		}
		return
	}
	if evidenceRank(proposal.Evidence) > evidenceRank(accumulator.candidate.Evidence) {
		accumulator.candidate.Evidence = proposal.Evidence
		accumulator.candidate.Reason = proposal.Reason
		accumulator.candidate.Confidence = proposal.Confidence
		accumulator.candidate.ConfidenceLevel = proposal.ConfidenceLevel
	}
}

func preferProposal(candidate, current MembershipProposal) bool {
	if evidenceRank(candidate.Evidence) != evidenceRank(current.Evidence) {
		return evidenceRank(candidate.Evidence) > evidenceRank(current.Evidence)
	}
	if candidate.Title != current.Title {
		return candidate.Title < current.Title
	}
	return candidate.MediaType < current.MediaType
}

func finalizeCandidates(accumulators map[candidateKey]*candidateAccumulator) []UniverseCandidate {
	candidates := make([]UniverseCandidate, 0, len(accumulators))
	for _, accumulator := range accumulators {
		identityKeys := make([]identityKey, 0, len(accumulator.members))
		for key := range accumulator.members {
			identityKeys = append(identityKeys, key)
		}
		sort.Slice(identityKeys, func(i, j int) bool {
			if identityKeys[i].provider != identityKeys[j].provider {
				return identityKeys[i].provider < identityKeys[j].provider
			}
			return identityKeys[i].externalID < identityKeys[j].externalID
		})
		accumulator.candidate.Memberships = make([]MembershipProposal, 0, len(identityKeys))
		for _, key := range identityKeys {
			accumulator.candidate.Memberships = append(accumulator.candidate.Memberships, accumulator.members[key])
		}
		if !accumulator.candidate.Existing && accumulator.candidate.NamingConfidenceLevel == ConfidenceNone {
			titles := make([]string, 0, len(accumulator.candidate.Memberships))
			for _, membership := range accumulator.candidate.Memberships {
				if !membership.Suppressed && membership.ConfidenceLevel == ConfidenceHigh {
					titles = append(titles, membership.Title)
				}
			}
			if safeTitle, confidence, level, evidence, ok := safeDiscoveryName(NormalizeTitle(accumulator.candidate.Title), titles); ok {
				accumulator.candidate.Title = safeTitle
				accumulator.candidate.NamingConfidence = confidence
				accumulator.candidate.NamingConfidenceLevel = level
				accumulator.candidate.NamingEvidence = evidence
			}
		}
		candidates = append(candidates, accumulator.candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].UniverseID != candidates[j].UniverseID {
			return candidates[i].UniverseID < candidates[j].UniverseID
		}
		if NormalizeTitle(candidates[i].Title) != NormalizeTitle(candidates[j].Title) {
			return NormalizeTitle(candidates[i].Title) < NormalizeTitle(candidates[j].Title)
		}
		left, right := firstIdentity(candidates[i]), firstIdentity(candidates[j])
		if left.provider != right.provider {
			return left.provider < right.provider
		}
		return left.externalID < right.externalID
	})
	if len(candidates) > MaxUniverseCandidates {
		candidates = candidates[:MaxUniverseCandidates]
	}
	return candidates
}

func firstIdentity(candidate UniverseCandidate) identityKey {
	if len(candidate.Memberships) == 0 {
		return identityKey{}
	}
	return identityKey{provider: candidate.Memberships[0].Provider, externalID: candidate.Memberships[0].ExternalID}
}

func tokens(normalized string) []string {
	if normalized == "" {
		return nil
	}
	return strings.Fields(normalized)
}

func distinctiveTokens(input []string) []string {
	var distinctive []string
	for _, token := range input {
		if isDistinctiveToken(token) {
			distinctive = append(distinctive, token)
		}
	}
	return distinctive
}

func isDistinctiveToken(token string) bool {
	if utf8.RuneCountInString(token) < 5 {
		return false
	}
	switch token {
	case "about", "after", "again", "also", "among", "book", "books", "cars", "chapter", "classic", "collection", "detective", "episode", "film", "films", "for", "from", "game", "games", "great", "house", "movie", "movies", "new", "part", "season", "series", "show", "story", "the", "their", "there", "these", "this", "those", "through", "volume", "world":
		return false
	default:
		return true
	}
}

func hasDistinctiveWholeToken(distinctive, titleTokens []string) bool {
	for _, anchor := range distinctive {
		for _, token := range titleTokens {
			if anchor == token {
				return true
			}
		}
	}
	return false
}

func sharesDistinctiveToken(left, right, distinctive []string) bool {
	for _, token := range distinctive {
		if containsToken(left, token) && containsToken(right, token) {
			return true
		}
	}
	return false
}

func containsToken(tokens []string, target string) bool {
	for _, token := range tokens {
		if token == target {
			return true
		}
	}
	return false
}

func containsTokenSequence(haystack, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for start := 0; start <= len(haystack)-len(needle); start++ {
		matches := true
		for offset, token := range needle {
			if haystack[start+offset] != token {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func evidenceRank(evidence EvidenceType) int {
	switch evidence {
	case EvidenceConfirmedIdentity:
		return 5
	case EvidenceExistingMembership:
		return 4
	case EvidenceExactTitleAnchor:
		return 3
	case EvidenceWholePhraseAnchor:
		return 3
	case EvidenceExplicitAlias:
		return 2
	case EvidenceDistinctiveToken:
		return 1
	default:
		return 0
	}
}

func confidenceFor(evidence EvidenceType) (float64, ConfidenceLevel) {
	switch evidence {
	case EvidenceConfirmedIdentity, EvidenceUserConfirmedMembership:
		return 1.0, ConfidenceHigh
	case EvidenceExactTitleAnchor:
		return 0.95, ConfidenceHigh
	case EvidenceWholePhraseAnchor:
		return 0.95, ConfidenceHigh
	case EvidenceExplicitAlias:
		return 0.9, ConfidenceHigh
	case EvidenceExistingMembership:
		return 1.0, ConfidenceHigh
	case EvidenceDistinctiveToken:
		return 0.8, ConfidenceModerate
	default:
		return 0, ConfidenceNone
	}
}

func reasonFor(evidence EvidenceType) string {
	switch evidence {
	case EvidenceConfirmedIdentity:
		return "This exact provider identity already has a user-confirmed membership in the Universe."
	case EvidenceUserConfirmedMembership:
		return "The user explicitly accepted this exact Work for the Universe."
	case EvidenceExistingMembership:
		return "This exact provider identity already has an accepted membership in the Universe."
	case EvidenceExactTitleAnchor:
		return "The normalized user query exactly matches the result title and supplies this candidate's title anchor."
	case EvidenceWholePhraseAnchor:
		return "The complete normalized query phrase appears as consecutive title tokens."
	case EvidenceExplicitAlias:
		return "The normalized user query matches a stored Universe alias present as a whole-token title phrase."
	case EvidenceDistinctiveToken:
		return "A distinctive normalized query token appears as a whole title token; no substring match is used."
	case EvidenceExplicitExclusion:
		return "An explicit exclusion blocks this exact Work/Universe pair from being proposed again."
	default:
		return "No supported matching evidence was found."
	}
}

func validEvidenceType(evidence EvidenceType) bool {
	switch evidence {
	case EvidenceConfirmedIdentity, EvidenceUserConfirmedMembership, EvidenceExistingMembership, EvidenceExactTitleAnchor, EvidenceWholePhraseAnchor,
		EvidenceExplicitAlias, EvidenceDistinctiveToken, EvidenceExplicitExclusion:
		return true
	default:
		return false
	}
}

func confidenceLevelFromDomain(level domain.UniverseNamingConfidenceLevel) ConfidenceLevel {
	switch level {
	case domain.UniverseNamingConfidenceHigh:
		return ConfidenceHigh
	case domain.UniverseNamingConfidenceModerate:
		return ConfidenceModerate
	default:
		return ConfidenceNone
	}
}
