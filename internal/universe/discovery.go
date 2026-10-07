package universe

import (
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"lernae/internal/domain"
)

// DiscoveryCandidate is a bounded, high-confidence proposal for automatic
// materialization. Universe existence confidence is independent of each
// exact Work membership confidence.
type DiscoveryCandidate struct {
	QueryKey              string
	Title                 string
	ExistenceConfidence   float64
	ConfidenceLevel       ConfidenceLevel
	NamingConfidence      float64
	NamingConfidenceLevel ConfidenceLevel
	NamingEvidence        domain.UniverseNamingEvidence
	Memberships           []DiscoveryMembership
	UnrelatedResultCount  int
}

// DiscoveryMembership carries only allowlisted Work identity and display data.
// It never contains provider payloads or provider score values.
type DiscoveryMembership struct {
	Work            domain.WorkMaterialization
	Evidence        EvidenceType
	Reason          string
	Confidence      float64
	ConfidenceLevel ConfidenceLevel
}

type discoveryResult struct {
	result         domain.MetadataSearchResult
	existenceMatch bool
	membership     *DiscoveryMembership
	ambiguous      bool
}

// AssessAutomaticDiscovery returns a candidate only when bounded title
// evidence clears conservative cross-result thresholds. It is a lexical
// evidence gate, not a franchise allowlist or a persistence operation.
func AssessAutomaticDiscovery(query string, results []domain.MetadataSearchResult) (DiscoveryCandidate, bool) {
	if len(query) == 0 || len(query) > 200 || len(results) == 0 || len(results) > MaxCandidateResults {
		return DiscoveryCandidate{}, false
	}
	queryKey := NormalizeTitle(query)
	if utf8.RuneCountInString(queryKey) > 200 {
		return DiscoveryCandidate{}, false
	}
	queryTokens := tokens(queryKey)
	if len(queryTokens) == 0 || isAmbiguousAutomaticQuery(queryTokens) {
		return DiscoveryCandidate{}, false
	}
	canonical := canonicalResults(results)
	identities := make(map[identityKey]discoveryResult, len(canonical))
	for _, result := range canonical {
		if !validDiscoveryResult(result) {
			continue
		}
		key := identityKey{provider: result.Provider, externalID: result.ExternalID}
		evidence, matched := discoveryTitleEvidence(queryKey, queryTokens, result)
		current, found := identities[key]
		if !found {
			current.result = result
			current.existenceMatch = matched
			// Weak/Related title evidence can corroborate the Universe, but only
			// Best results qualify as independently confident memberships.
			if matched && result.Relevance == domain.SearchRelevanceBest {
				membership := highConfidenceMembership(result, evidence)
				current.membership = &membership
			}
			identities[key] = current
			continue
		}
		if current.result.Medium != result.Medium {
			current.ambiguous = true
			current.membership = nil
			identities[key] = current
			continue
		}
		if matched {
			if !current.existenceMatch {
				current.result = result
			}
			current.existenceMatch = true
			if result.Relevance == domain.SearchRelevanceBest {
				membership := highConfidenceMembership(result, evidence)
				if current.membership == nil || strongerDiscoveryMembership(membership, *current.membership) {
					current.membership = &membership
				}
			}
		}
		identities[key] = current
	}

	memberships := make([]DiscoveryMembership, 0, len(identities))
	existenceEvidence := make([]domain.MetadataSearchResult, 0, len(identities))
	unrelatedResultCount := 0
	for _, identity := range identities {
		if identity.ambiguous {
			unrelatedResultCount++
			continue
		}
		if identity.existenceMatch {
			existenceEvidence = append(existenceEvidence, identity.result)
		} else {
			unrelatedResultCount++
		}
		if identity.membership != nil {
			memberships = append(memberships, *identity.membership)
		}
	}
	sort.Slice(memberships, func(i, j int) bool {
		if memberships[i].Work.Provider != memberships[j].Work.Provider {
			return memberships[i].Work.Provider < memberships[j].Work.Provider
		}
		return memberships[i].Work.ExternalID < memberships[j].Work.ExternalID
	})
	if !automaticDiscoveryThreshold(queryTokens, existenceEvidence, unrelatedResultCount) {
		return DiscoveryCandidate{}, false
	}
	titles := make([]string, 0, len(memberships))
	for _, membership := range memberships {
		titles = append(titles, membership.Work.Title)
	}
	title, namingConfidence, namingLevel, namingEvidence, safeName := safeDiscoveryName(queryKey, titles)
	if !safeName {
		return DiscoveryCandidate{}, false
	}
	media := make(map[domain.Medium]struct{})
	providers := make(map[string]struct{})
	for _, result := range existenceEvidence {
		media[result.Medium] = struct{}{}
		providers[result.Provider] = struct{}{}
	}
	existenceConfidence := 0.9
	if len(providers) >= 2 {
		existenceConfidence = 0.95
	}
	if len(media) >= 2 && len(providers) >= 2 {
		existenceConfidence = 0.98
	}
	return DiscoveryCandidate{
		QueryKey: queryKey, Title: title,
		ExistenceConfidence: existenceConfidence, ConfidenceLevel: ConfidenceHigh,
		NamingConfidence: namingConfidence, NamingConfidenceLevel: namingLevel, NamingEvidence: namingEvidence,
		Memberships: memberships, UnrelatedResultCount: unrelatedResultCount,
	}, true
}

// IsDistinctiveSingleTokenQuery reports whether a normalized one-token query
// can safely use token containment as local discovery evidence. Exact title
// and alias matches do not depend on this guard.
func IsDistinctiveSingleTokenQuery(query string) bool {
	queryTokens := tokens(NormalizeTitle(query))
	return len(queryTokens) == 1 && isDistinctiveToken(queryTokens[0])
}

// HasDistinctiveDiscoveryToken reports whether a normalized query contains at
// least one token specific enough to support stable discovery-key lookup.
func HasDistinctiveDiscoveryToken(query string) bool {
	return len(distinctiveTokens(tokens(NormalizeTitle(query)))) > 0
}

func isAmbiguousAutomaticQuery(queryTokens []string) bool {
	if len(queryTokens) != 1 {
		return false
	}
	token := queryTokens[0]
	if !isDistinctiveToken(token) {
		return true
	}
	// These isolated terms are known to be ambiguous across unrelated catalog
	// domains. This is a fail-closed query guard, not a franchise allowlist.
	switch token {
	case "conan":
		return true
	default:
		return false
	}
}

func validDiscoveryResult(result domain.MetadataSearchResult) bool {
	return strings.TrimSpace(result.Provider) != "" && utf8.RuneCountInString(result.Provider) <= 128 &&
		strings.TrimSpace(result.ExternalID) != "" && utf8.RuneCountInString(result.ExternalID) <= 512 &&
		strings.TrimSpace(result.Title) != "" && utf8.RuneCountInString(result.Title) <= 512 && result.Medium.Valid() &&
		strings.TrimSpace(result.WorkType) != "" && utf8.RuneCountInString(result.WorkType) <= 128
}

func discoveryTitleEvidence(queryKey string, queryTokens []string, result domain.MetadataSearchResult) (EvidenceType, bool) {
	title := NormalizeTitle(result.Title)
	switch {
	case title == queryKey:
		return EvidenceExactTitleAnchor, true
	case len(queryTokens) == 1 && containsToken(tokens(title), queryTokens[0]):
		return EvidenceDistinctiveToken, true
	case len(queryTokens) > 1 && hasStrongPhraseAnchor(tokens(title), queryTokens):
		return EvidenceWholePhraseAnchor, true
	}
	for _, alias := range result.Aliases {
		aliasKey := NormalizeTitle(alias)
		aliasMatches := aliasKey == queryKey || (len(queryTokens) == 1 && containsToken(tokens(aliasKey), queryTokens[0]))
		if len(queryTokens) > 1 && hasStrongPhraseAnchor(tokens(aliasKey), queryTokens) {
			aliasMatches = true
		}
		if aliasMatches {
			return EvidenceExplicitAlias, true
		}
	}
	return "", false
}

// hasStrongPhraseAnchor requires a leading phrase when a query has only one
// distinctive token, because its common words make embedded matches ambiguous.
// Queries with multiple distinctive anchors retain whole-phrase matching.
func hasStrongPhraseAnchor(titleTokens, queryTokens []string) bool {
	if hasMultiplePhraseAnchors(queryTokens) && containsTokenSequence(titleTokens, queryTokens) {
		return true
	}
	if startsWithPhrase(titleTokens, queryTokens) {
		return true
	}
	if len(titleTokens) > 1 && isDiscoveryArticle(titleTokens[0]) {
		return startsWithPhrase(titleTokens[1:], queryTokens)
	}
	return false
}

// Phrase anchors filter common function words and media boilerplate, but retain
// informative title words that the isolated-query guard may treat cautiously.
func hasMultiplePhraseAnchors(queryTokens []string) bool {
	anchors := 0
	for _, token := range queryTokens {
		if genericPhraseAnchor(token) {
			continue
		}
		anchors++
		if anchors == 2 {
			return true
		}
	}
	return false
}

func genericPhraseAnchor(token string) bool {
	switch token {
	case "a", "an", "and", "as", "at", "by", "for", "from", "in", "into", "is", "of", "on", "or", "the", "to", "with", "without",
		"book", "books", "chapter", "chapters", "episode", "episodes", "film", "films", "game", "games", "movie", "movies", "novel", "novels", "part", "season", "series", "vol", "volume",
		"el", "la", "las", "los", "un", "una", "de", "del", "y", "en", "por", "para", "con", "libro", "juego", "pelicula", "película", "serie", "temporada":
		return true
	default:
		return false
	}
}

func startsWithPhrase(titleTokens, queryTokens []string) bool {
	if len(queryTokens) == 0 || len(queryTokens) > len(titleTokens) {
		return false
	}
	for index, token := range queryTokens {
		if titleTokens[index] != token {
			return false
		}
	}
	return true
}

func isDiscoveryArticle(token string) bool {
	switch token {
	case "a", "an", "the":
		return true
	default:
		return false
	}
}

func highConfidenceMembership(result domain.MetadataSearchResult, evidence EvidenceType) DiscoveryMembership {
	confidence := 0.9
	switch evidence {
	case EvidenceExactTitleAnchor:
		confidence = 1.0
	case EvidenceWholePhraseAnchor:
		confidence = 0.95
	}
	return DiscoveryMembership{
		Work: domain.WorkMaterialization{
			Provider: result.Provider, ExternalID: result.ExternalID, Title: result.Title,
			Medium: result.Medium, WorkType: result.WorkType,
		},
		Evidence: evidence, Reason: reasonFor(evidence), Confidence: confidence, ConfidenceLevel: ConfidenceHigh,
	}
}

func strongerDiscoveryMembership(candidate, current DiscoveryMembership) bool {
	if candidate.Confidence != current.Confidence {
		return candidate.Confidence > current.Confidence
	}
	left, right := NormalizeTitle(candidate.Work.Title), NormalizeTitle(current.Work.Title)
	if left != right {
		return left < right
	}
	return candidate.Work.Title < current.Work.Title
}

func automaticDiscoveryThreshold(queryTokens []string, evidence []domain.MetadataSearchResult, unrelatedCount int) bool {
	media := make(map[domain.Medium]struct{})
	providers := make(map[string]struct{})
	for _, result := range evidence {
		media[result.Medium] = struct{}{}
		providers[result.Provider] = struct{}{}
	}
	if len(queryTokens) == 1 {
		return len(evidence) >= 3 && len(media) >= 2 && unrelatedCount < len(evidence)
	}
	return len(evidence) >= 2 && (len(media) >= 2 || len(providers) >= 2)
}

func safeDiscoveryName(queryKey string, titles []string) (string, float64, ConfidenceLevel, domain.UniverseNamingEvidence, bool) {
	queryTokens := tokens(queryKey)
	if len(distinctiveTokens(queryTokens)) > 0 && !isAmbiguousAutomaticQuery(queryTokens) {
		return formatNormalizedTitle(queryKey), 0.95, ConfidenceHigh, domain.UniverseNamingEvidenceDistinctiveQuery, true
	}
	if anchor, ok := sharedHighConfidenceAnchor(titles); ok {
		return formatNormalizedTitle(anchor), 0.9, ConfidenceHigh, domain.UniverseNamingEvidenceSharedAnchor, true
	}
	return "", 0, ConfidenceNone, domain.UniverseNamingEvidenceLegacyUnknown, false
}

func sharedHighConfidenceAnchor(titles []string) (string, bool) {
	counts := make(map[string]int)
	for _, title := range titles {
		workTokens := tokens(NormalizeTitle(title))
		seen := make(map[string]struct{})
		for start := range workTokens {
			for end := start + 1; end <= len(workTokens); end++ {
				anchorTokens := workTokens[start:end]
				if len(distinctiveTokens(anchorTokens)) == 0 {
					continue
				}
				seen[strings.Join(anchorTokens, " ")] = struct{}{}
			}
		}
		for anchor := range seen {
			counts[anchor]++
		}
	}
	var best string
	for anchor, count := range counts {
		if count < 2 {
			continue
		}
		if best == "" || betterSharedAnchor(anchor, best, count, counts[best]) {
			best = anchor
		}
	}
	return best, best != ""
}

func betterSharedAnchor(candidate, current string, candidateCount, currentCount int) bool {
	candidateTokens, currentTokens := len(tokens(candidate)), len(tokens(current))
	if candidateTokens != currentTokens {
		return candidateTokens > currentTokens
	}
	if candidateCount != currentCount {
		return candidateCount > currentCount
	}
	return candidate < current
}

func formatNormalizedTitle(normalized string) string {
	words := tokens(NormalizeTitle(normalized))
	for index, word := range words {
		if index > 0 && index < len(words)-1 && isMinorTitleWord(word) {
			continue
		}
		words[index] = cases.Title(language.English).String(word)
	}
	return strings.Join(words, " ")
}

func isMinorTitleWord(word string) bool {
	switch word {
	case "a", "an", "and", "at", "by", "for", "in", "of", "on", "the", "to":
		return true
	default:
		return false
	}
}
