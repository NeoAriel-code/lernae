// Package tvmaze adapts TVMaze's official search endpoint to Lernae's
// provider-neutral search contract.
package tvmaze

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"lernae/internal/domain"
	"lernae/internal/providers"
)

const (
	SearchEndpoint       = "https://api.tvmaze.com/search/shows"
	MaxResponseBodyBytes = providers.MaxSearchResponseBytes
	defaultTimeout       = 5 * time.Second
	defaultRetryBackoff  = 2 * time.Second
	defaultRateInterval  = 500 * time.Millisecond
	userAgent            = "Lernae/0.1"
)

var (
	ErrInvalidQuery                            = providers.ErrSearchInvalid
	ErrSearchThrottled                         = providers.ErrSearchThrottled
	ErrSearchUnavailable                       = providers.ErrSearchUnavailable
	ErrMalformedResponse                       = providers.ErrSearchMalformed
	ErrResponseTooLarge                        = providers.ErrSearchBodyTooLarge
	ErrUnsupportedProvider                     = providers.ErrDetailsUnsupportedProvider
	ErrInvalidExternalID                       = providers.ErrDetailsInvalidExternalID
	ErrWorkNotFound                            = providers.ErrDetailsWorkNotFound
	ErrDetailsUnavailable                      = providers.ErrDetailsProviderUnavailable
	ErrDetailsMalformedResponse                = providers.ErrDetailsMalformedResponse
	ErrDetailsResponseTooLarge                 = providers.ErrDetailsResponseTooLarge
	ErrDetailsRateLimited                      = providers.ErrDetailsRateLimited
	processRateLimiter          RequestLimiter = newRateLimiter(defaultRateInterval)
)

type ClientConfig struct {
	Endpoint       string
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	RetryBackoff   time.Duration
	Limiter        RequestLimiter
}

type RequestLimiter interface {
	Wait(context.Context) error
}

type SearchSource struct {
	endpoint       string
	httpClient     *http.Client
	requestTimeout time.Duration
	retryBackoff   time.Duration
	limiter        RequestLimiter
}

type searchHit struct {
	Show showResponse `json:"show"`
}

type showResponse struct {
	ID         int64            `json:"id"`
	Name       string           `json:"name"`
	URL        string           `json:"url"`
	Type       string           `json:"type"`
	Language   string           `json:"language"`
	Status     string           `json:"status"`
	Genres     []string         `json:"genres"`
	Premiered  string           `json:"premiered"`
	Summary    string           `json:"summary"`
	Image      *imageResponse   `json:"image"`
	Network    *channelResponse `json:"network"`
	WebChannel *channelResponse `json:"webChannel"`
}

type channelResponse struct {
	Name string `json:"name"`
}

type imageResponse struct {
	Medium   string `json:"medium"`
	Original string `json:"original"`
}

type rateLimiter struct {
	gate     chan struct{}
	interval time.Duration
	next     time.Time
}

var _ providers.SearchProvider = (*SearchSource)(nil)
var _ providers.WorkDetailsSource = (*SearchSource)(nil)

func newRateLimiter(interval time.Duration) *rateLimiter {
	return &rateLimiter{gate: make(chan struct{}, 1), interval: interval}
}

func (limiter *rateLimiter) Wait(ctx context.Context) error {
	select {
	case limiter.gate <- struct{}{}:
		defer func() { <-limiter.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if delay := limiter.next.Sub(time.Now()); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	limiter.next = time.Now().Add(limiter.interval)
	return nil
}

func New(config ClientConfig) *SearchSource {
	endpoint := config.Endpoint
	if endpoint == "" {
		endpoint = SearchEndpoint
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	client := *config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = defaultTimeout
	}
	if config.RetryBackoff <= 0 {
		config.RetryBackoff = defaultRetryBackoff
	}
	if config.Limiter == nil {
		config.Limiter = processRateLimiter
	}
	return &SearchSource{
		endpoint: endpoint, httpClient: &client, requestTimeout: config.RequestTimeout,
		retryBackoff: config.RetryBackoff, limiter: config.Limiter,
	}
}

func (*SearchSource) Name() string { return "tvmaze" }

func (source *SearchSource) Search(ctx context.Context, query string, limit int) ([]domain.MetadataSearchResult, error) {
	if err := providers.ValidateSearchRequest(query, limit); err != nil {
		return nil, ErrInvalidQuery
	}
	requestCtx, cancel := context.WithTimeout(ctx, source.requestTimeout)
	defer cancel()
	target, err := searchURL(source.endpoint, strings.TrimSpace(query))
	if err != nil {
		return nil, ErrSearchUnavailable
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := source.limiter.Wait(requestCtx); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target, nil)
		if err != nil {
			return nil, ErrSearchUnavailable
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", userAgent)
		response, err := source.httpClient.Do(request)
		if err != nil {
			if requestCtx.Err() != nil {
				return nil, requestCtx.Err()
			}
			return nil, ErrSearchUnavailable
		}
		if response.StatusCode == http.StatusTooManyRequests {
			response.Body.Close()
			if attempt == 1 {
				return nil, ErrSearchThrottled
			}
			if err := waitContext(requestCtx, source.retryBackoff); err != nil {
				return nil, err
			}
			continue
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, ErrSearchUnavailable
		}
		body, readErr := readBounded(response.Body, MaxResponseBodyBytes)
		response.Body.Close()
		if readErr != nil {
			if requestCtx.Err() != nil {
				return nil, requestCtx.Err()
			}
			return nil, readErr
		}
		var payload []searchHit
		if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
			return nil, ErrMalformedResponse
		}
		return normalizeShows(payload, limit), nil
	}
	return nil, ErrSearchThrottled
}

func (source *SearchSource) GetWorkDetails(ctx context.Context, provider, externalID string) (domain.WorkDetails, error) {
	if provider != "tvmaze" {
		return domain.WorkDetails{}, ErrUnsupportedProvider
	}
	if !validShowID(externalID) {
		return domain.WorkDetails{}, ErrInvalidExternalID
	}
	requestCtx, cancel := context.WithTimeout(ctx, source.requestTimeout)
	defer cancel()
	if err := source.limiter.Wait(requestCtx); err != nil {
		return domain.WorkDetails{}, err
	}
	target, err := detailsURL(source.endpoint, externalID)
	if err != nil {
		return domain.WorkDetails{}, ErrDetailsUnavailable
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target, nil)
	if err != nil {
		return domain.WorkDetails{}, ErrDetailsUnavailable
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)
	response, err := source.httpClient.Do(request)
	if err != nil {
		if requestCtx.Err() != nil {
			return domain.WorkDetails{}, requestCtx.Err()
		}
		return domain.WorkDetails{}, ErrDetailsUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return domain.WorkDetails{}, ErrWorkNotFound
	case http.StatusTooManyRequests:
		return domain.WorkDetails{}, ErrDetailsRateLimited
	default:
		return domain.WorkDetails{}, ErrDetailsUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBodyBytes+1))
	if err != nil {
		if requestCtx.Err() != nil {
			return domain.WorkDetails{}, requestCtx.Err()
		}
		return domain.WorkDetails{}, ErrDetailsUnavailable
	}
	if int64(len(body)) > MaxResponseBodyBytes {
		return domain.WorkDetails{}, ErrDetailsResponseTooLarge
	}
	var show showResponse
	if err := json.Unmarshal(body, &show); err != nil || show.ID <= 0 || strconv.FormatInt(show.ID, 10) != externalID || sanitizeSummary(show.Name) == "" {
		return domain.WorkDetails{}, ErrDetailsMalformedResponse
	}
	title := sanitizeSummary(show.Name)
	details := domain.WorkDetails{
		Provider: "tvmaze", ExternalID: externalID, Title: title,
		Summary: sanitizeSummary(show.Summary), CoverReference: safeArtwork(show.Image),
		SourceURL: safeSourceURL(externalID, show.URL), Language: normalizeTVLanguage(show.Language),
		Status: sanitizeSummary(show.Status), Genres: sanitizeGenres(show.Genres),
		Network: channelName(show.Network), WebChannel: channelName(show.WebChannel),
		Medium: domain.MediumVideo, WorkType: "series", PlatformCandidates: []domain.PlatformCandidate{},
	}
	if premiered, err := time.Parse("2006-01-02", strings.TrimSpace(show.Premiered)); err == nil {
		premiered = premiered.UTC()
		details.ReleaseDate = &premiered
		details.ReleaseYear = premiered.Year()
	}
	if seasonSummary, err := source.GetSeasonSummary(ctx, externalID); err == nil &&
		seasonSummary.SeasonCount >= 0 && seasonSummary.SeasonCount <= maxKnownSeasons {
		seasonCount := seasonSummary.SeasonCount
		details.SeasonCount = &seasonCount
	}
	return details, nil
}

func validShowID(externalID string) bool {
	if externalID == "" || len(externalID) > 19 {
		return false
	}
	id, err := strconv.ParseInt(externalID, 10, 64)
	return err == nil && id > 0 && strconv.FormatInt(id, 10) == externalID
}

func detailsURL(endpoint, externalID string) (string, error) {
	target, err := url.Parse(endpoint)
	if err != nil || target.Host == "" || (target.Scheme != "https" && target.Scheme != "http") {
		return "", errors.New("invalid TVMaze endpoint")
	}
	target.Path = "/shows/" + externalID
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	return target.String(), nil
}

func channelName(channel *channelResponse) string {
	if channel == nil {
		return ""
	}
	return sanitizeSummary(channel.Name)
}

func sanitizeGenres(genres []string) []string {
	result := make([]string, 0, min(20, len(genres)))
	seen := make(map[string]struct{}, len(genres))
	for _, genre := range genres {
		genre = sanitizeSummary(genre)
		if genre == "" {
			continue
		}
		key := strings.ToLower(genre)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, genre)
		if len(result) == 20 {
			break
		}
	}
	return result
}

func normalizeTVLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	if len(language) == 2 && language[0] >= 'a' && language[0] <= 'z' && language[1] >= 'a' && language[1] <= 'z' {
		return language
	}
	return map[string]string{
		"english": "en", "spanish": "es", "french": "fr", "german": "de", "italian": "it",
		"portuguese": "pt", "japanese": "ja", "chinese": "zh", "russian": "ru", "arabic": "ar",
		"hindi": "hi", "korean": "ko", "dutch": "nl", "polish": "pl", "swedish": "sv",
		"norwegian": "no", "danish": "da", "finnish": "fi", "hebrew": "he", "turkish": "tr",
	}[language]
}

func searchURL(endpoint, query string) (string, error) {
	target, err := url.Parse(endpoint)
	if err != nil || target.Host == "" || (target.Scheme != "https" && target.Scheme != "http") {
		return "", errors.New("invalid TVMaze endpoint")
	}
	if target.Path == "" || target.Path == "/" {
		target.Path = "/search/shows"
	}
	values := target.Query()
	values.Set("q", query)
	target.RawQuery = values.Encode()
	return target.String(), nil
}

func normalizeShows(hits []searchHit, limit int) []domain.MetadataSearchResult {
	results := make([]domain.MetadataSearchResult, 0, min(limit, len(hits)))
	for _, hit := range hits {
		if len(results) >= limit || hit.Show.ID <= 0 {
			continue
		}
		title := strings.TrimSpace(hit.Show.Name)
		if title == "" {
			continue
		}
		showID := strconv.FormatInt(hit.Show.ID, 10)
		year := 0
		if len(hit.Show.Premiered) >= 4 {
			if parsed, err := strconv.Atoi(hit.Show.Premiered[:4]); err == nil && parsed > 0 {
				year = parsed
			}
		}
		artwork := safeArtwork(hit.Show.Image)
		result := domain.MetadataSearchResult{
			Provider: "tvmaze", ExternalID: showID, Title: title, Subtitle: strings.TrimSpace(hit.Show.Type),
			MediaType: "tv", Year: year, ReleaseYear: year, Summary: sanitizeSummary(hit.Show.Summary),
			Network: channelName(hit.Show.Network), WebChannel: channelName(hit.Show.WebChannel),
			ArtworkURL: artwork, CoverReference: artwork, SourceURL: safeSourceURL(showID, hit.Show.URL),
			Medium: domain.MediumVideo, WorkType: "series", SourceRank: len(results) + 1,
		}
		results = append(results, result)
	}
	return results
}

func safeSourceURL(showID, candidate string) string {
	fallback := "https://www.tvmaze.com/shows/" + showID
	parsed, err := url.Parse(strings.TrimSpace(candidate))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "www.tvmaze.com") ||
		parsed.Port() != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || path.Clean(parsed.Path) != parsed.Path {
		return fallback
	}
	showPath := "/shows/" + showID
	if parsed.Path != showPath && !strings.HasPrefix(parsed.Path, showPath+"/") {
		return fallback
	}
	return parsed.String()
}

func safeArtwork(image *imageResponse) string {
	if image == nil {
		return ""
	}
	for _, candidate := range []string{image.Original, image.Medium} {
		parsed, err := url.Parse(strings.TrimSpace(candidate))
		if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "static.tvmaze.com") ||
			parsed.Port() != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" ||
			!strings.HasPrefix(parsed.Path, "/uploads/images/") || path.Clean(parsed.Path) != parsed.Path {
			continue
		}
		return parsed.String()
	}
	return ""
}

func sanitizeSummary(summary string) string {
	plain := stripMarkup(html.UnescapeString(summary))
	plain = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) && !unicode.IsSpace(character) {
			return -1
		}
		return character
	}, plain)
	return strings.Join(strings.Fields(plain), " ")
}

func stripMarkup(input string) string {
	lower := strings.ToLower(input)
	var text strings.Builder
	for index := 0; index < len(input); {
		if strings.HasPrefix(input[index:], "<!--") {
			if end := strings.Index(input[index+4:], "-->"); end >= 0 {
				index += 4 + end + 3
				continue
			}
			break
		}
		name, ok := tagName(input, index)
		if !ok {
			text.WriteByte(input[index])
			index++
			continue
		}
		end, found := tagEnd(input, index+1)
		if !found {
			break
		}
		if name == "script" || name == "style" {
			closing := strings.Index(lower[end+1:], "</"+name)
			if closing < 0 {
				break
			}
			closingStart := end + 1 + closing
			closingEnd, closingFound := tagEnd(input, closingStart+2+len(name))
			if !closingFound {
				break
			}
			index = closingEnd + 1
			continue
		}
		index = end + 1
	}
	return text.String()
}

func tagName(input string, index int) (string, bool) {
	if index >= len(input) || input[index] != '<' {
		return "", false
	}
	start := index + 1
	if start < len(input) && input[start] == '/' {
		start++
	}
	if start >= len(input) || !isASCIIAlpha(input[start]) {
		return "", false
	}
	end := start + 1
	for end < len(input) && (isASCIIAlpha(input[end]) || input[end] == '-' || input[end] == ':') {
		end++
	}
	return strings.ToLower(input[start:end]), true
}

func tagEnd(input string, start int) (int, bool) {
	var quote byte
	for index := start; index < len(input); index++ {
		character := input[index]
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
		} else if character == '>' {
			return index, true
		}
	}
	return 0, false
}

func isASCIIAlpha(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func readBounded(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, ErrSearchUnavailable
	}
	if int64(len(data)) > limit {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}
