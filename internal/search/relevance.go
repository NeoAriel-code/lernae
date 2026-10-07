package search

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"lernae/internal/domain"
)

// normalizeSearchText applies Unicode compatibility normalization and full case
// folding, then treats punctuation and whitespace as token boundaries. It
// preserves accents and non-Latin letters rather than transliterating them.
func normalizeSearchText(value string) string {
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

// canonicalSearchTitle compares a complete title while ignoring only the
// whitespace and punctuation boundaries retained by normalizeSearchText.
// Exact equality here deliberately avoids substring matching.
func canonicalSearchTitle(value string) string {
	return strings.ReplaceAll(normalizeSearchText(value), " ", "")
}

func classifySearchRelevance(query string, result domain.MetadataSearchResult) domain.SearchRelevance {
	queryText := normalizeSearchText(query)
	if queryText == "" {
		return domain.SearchRelevanceWeak
	}
	queryTokens := strings.Fields(queryText)
	if len(distinctiveTokens(queryTokens)) == 0 {
		return domain.SearchRelevanceWeak
	}
	queryCanonical := canonicalSearchTitle(queryText)
	candidateTitles := make([]string, 0, len(result.Aliases)+1)
	candidateTitles = append(candidateTitles, normalizeSearchText(result.Title))
	for _, alias := range result.Aliases {
		candidateTitles = append(candidateTitles, normalizeSearchText(alias))
	}
	for _, candidateTitle := range candidateTitles {
		if candidateTitle != "" && canonicalSearchTitle(candidateTitle) == queryCanonical &&
			(!isGuideSearchResult(result) || hasGuideTitle(queryText)) {
			return domain.SearchRelevanceBest
		}
	}
	if isGameSearchResult(result) {
		for _, candidateTitle := range candidateTitles {
			if isFranchiseInstallmentVariant(queryText, candidateTitle) && !isGuideSearchResult(result) {
				return domain.SearchRelevanceBest
			}
		}
	}

	queryAnchors := distinctiveTokens(queryTokens)
	if len(queryTokens) > 1 && len(queryAnchors) > 0 {
		for _, candidateTitle := range candidateTitles {
			if (hasStrongPhraseAnchor(strings.Fields(candidateTitle), queryTokens) ||
				hasExplicitAliasPhraseAnchor(candidateTitle, result.Aliases, queryTokens)) &&
				!isFranchiseInstallmentVariant(queryText, candidateTitle) &&
				!isGuideSearchResult(result) {
				return domain.SearchRelevanceBest
			}
		}
	}

	if len(queryAnchors) < 2 {
		return domain.SearchRelevanceWeak
	}
	shared := make(map[string]struct{}, len(queryAnchors))
	for _, candidateTitle := range candidateTitles {
		for token := range distinctiveTokens(strings.Fields(candidateTitle)) {
			if _, found := queryAnchors[token]; found {
				shared[token] = struct{}{}
			}
		}
	}
	if len(shared) >= 2 {
		return domain.SearchRelevanceRelated
	}
	return domain.SearchRelevanceWeak
}

// hasStrongPhraseAnchor requires the full query phrase to lead the title (or
// follow one leading article). Embedded phrase collisions are related evidence,
// not enough to put a result in Best.
func hasStrongPhraseAnchor(titleTokens, queryTokens []string) bool {
	if startsWithTokenSequence(titleTokens, queryTokens) {
		return true
	}
	if len(titleTokens) > 1 && isTitleArticle(titleTokens[0]) && len(titleTokens)-1 == len(queryTokens) {
		return startsWithTokenSequence(titleTokens[1:], queryTokens)
	}
	return false
}

func isGameSearchResult(result domain.MetadataSearchResult) bool {
	if result.Medium == domain.MediumGame {
		return true
	}
	for _, kind := range []string{result.MediaType, result.WorkType} {
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "game", "video game", "videogame":
			return true
		}
	}
	return false
}

func isCoreGameSearchMatch(query string, result domain.MetadataSearchResult) bool {
	if !isGameSearchResult(result) {
		return false
	}
	queryText := normalizeSearchText(query)
	if queryText == "" {
		return false
	}
	resultTitle := normalizeSearchText(result.Title)
	return canonicalSearchTitle(queryText) == canonicalSearchTitle(resultTitle) ||
		isFranchiseInstallmentVariant(queryText, resultTitle)
}

// isFranchiseInstallmentVariant accepts a complete query title followed by
// one explicit, conventional game installment number. It does not accept an
// arbitrary prefix, suffix, or substring collision.
func isFranchiseInstallmentVariant(queryText, candidateTitle string) bool {
	queryTokens := strings.Fields(queryText)
	candidateTokens := strings.Fields(candidateTitle)
	if len(queryTokens) == 0 || len(candidateTokens) == 0 {
		return false
	}

	var suffix []string
	switch {
	case startsWithTokenSequence(candidateTokens, queryTokens):
		suffix = candidateTokens[len(queryTokens):]
	case candidateTokens[0] == canonicalSearchTitle(queryText):
		suffix = candidateTokens[1:]
	}
	if len(suffix) > 0 && isGameInstallmentNumber(suffix[0]) {
		return true
	}

	// Some providers emit a compact franchise title and sequel marker as one token.
	// Accept only when the entire remainder is one known installment marker.
	queryCanonical := canonicalSearchTitle(queryText)
	candidateCanonical := canonicalSearchTitle(candidateTitle)
	remainder, found := strings.CutPrefix(candidateCanonical, queryCanonical)
	return found && isGameInstallmentNumber(remainder)
}

func isGameInstallmentNumber(token string) bool {
	switch strings.ToLower(token) {
	case "1", "2", "3", "4", "5", "6", "i", "ii", "iii", "iv", "v", "vi":
		return true
	default:
		return false
	}
}

func hasGuideTitle(candidateTitle string) bool {
	for _, token := range strings.Fields(candidateTitle) {
		switch token {
		case "guide", "guides", "handbook", "manual", "strategy", "walkthrough", "walkthroughs":
			return true
		}
	}
	return false
}

func isGuideSearchResult(result domain.MetadataSearchResult) bool {
	if hasGuideTitle(normalizeSearchText(result.Title)) {
		return true
	}
	for _, kind := range []string{result.MediaType, result.WorkType} {
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "guide", "strategy guide", "handbook", "manual", "walkthrough":
			return true
		}
	}
	return false
}

func startsWithTokenSequence(haystack, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for index, token := range needle {
		if haystack[index] != token {
			return false
		}
	}
	return true
}

func containsTokenSequence(haystack, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for start := 0; start <= len(haystack)-len(needle); start++ {
		if startsWithTokenSequence(haystack[start:], needle) {
			return true
		}
	}
	return false
}

// hasExplicitAliasPhraseAnchor preserves sequel/subtitle titles such as
// "Case Closed: Detective Conan" only when the leading title is a provider
// supplied alias. An embedded phrase alone is not a Best match.
func hasExplicitAliasPhraseAnchor(candidateTitle string, aliases, queryTokens []string) bool {
	titleTokens := strings.Fields(candidateTitle)
	for _, alias := range aliases {
		aliasTokens := strings.Fields(normalizeSearchText(alias))
		if len(aliasTokens) == 0 || len(aliasTokens) >= len(titleTokens) || !startsWithTokenSequence(titleTokens, aliasTokens) {
			continue
		}
		if containsTokenSequence(titleTokens[len(aliasTokens):], queryTokens) {
			return true
		}
	}
	return false
}

func isTitleArticle(token string) bool {
	switch token {
	case "a", "an", "the":
		return true
	default:
		return false
	}
}

func distinctiveTokens(tokens []string) map[string]struct{} {
	result := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if !genericSearchToken(token) {
			result[token] = struct{}{}
		}
	}
	return result
}

// genericSearchToken filters common function words and media boilerplate. It
// is intentionally not a franchise or provider-specific vocabulary.
func genericSearchToken(token string) bool {
	switch token {
	case "a", "an", "and", "as", "at", "by", "for", "from", "in", "into", "is", "of", "on", "or", "the", "to", "with", "without",
		"book", "books", "chapter", "chapters", "episode", "episodes", "film", "films", "game", "games", "movie", "movies", "novel", "novels", "part", "season", "series", "vol", "volume",
		"el", "la", "las", "los", "un", "una", "de", "del", "y", "en", "por", "para", "con", "libro", "juego", "pelicula", "película", "serie", "temporada":
		return true
	default:
		return false
	}
}
