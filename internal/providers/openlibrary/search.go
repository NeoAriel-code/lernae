// Package openlibrary adapts the official Open Library Search API to Lernae's
// provider-neutral search contract.
package openlibrary

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"lernae/internal/domain"
	"lernae/internal/providers"
)

const (
	SearchEndpoint            = "https://openlibrary.org/search.json"
	MaxResponseBodyBytes      = providers.MaxSearchResponseBytes
	DefaultMetadataLanguage   = "en"
	defaultTimeout            = 3 * time.Second
	defaultRateInterval       = time.Second
	userAgent                 = "Lernae/0.1"
	maxKnownEditions          = 100
	maxRepresentativeEditions = 10
	maxNormalizedText         = 16 << 10
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
	workKeyPattern                             = regexp.MustCompile(`^/works/(OL[0-9]+W)$`)
	editionKeyPattern                          = regexp.MustCompile(`^/books/(OL[0-9]+M)$`)
	workIDPattern                              = regexp.MustCompile(`^OL[0-9]+W$`)
	processRateLimiter          RequestLimiter = newRateLimiter(defaultRateInterval)
)

type ClientConfig struct {
	Endpoint         string
	HTTPClient       *http.Client
	RequestTimeout   time.Duration
	Limiter          RequestLimiter
	MetadataLanguage string
}

type SearchSource struct {
	endpoint           string
	httpClient         *http.Client
	requestTimeout     time.Duration
	limiter            RequestLimiter
	metadataLanguageMu sync.RWMutex
	metadataLanguage   string
	knownEditionMu     sync.Mutex
	knownEditions      map[string]domain.RepresentativeEdition
	knownEditionOrder  []string
}

var _ providers.SearchProvider = (*SearchSource)(nil)
var _ providers.WorkDetailsSource = (*SearchSource)(nil)

type searchResponse struct {
	Docs []searchDocument `json:"docs"`
}

type searchEditions struct {
	Docs []searchEdition `json:"docs"`
}

type searchDocument struct {
	Key              string         `json:"key"`
	Title            string         `json:"title"`
	AuthorNames      []string       `json:"author_name"`
	FirstPublishYear int            `json:"first_publish_year"`
	CoverID          int64          `json:"cover_i"`
	Editions         searchEditions `json:"editions"`
}

type searchEdition struct {
	Key         string   `json:"key"`
	Title       string   `json:"title"`
	Languages   []string `json:"language"`
	PublishDate string   `json:"publish_date"`
	CoverID     int64    `json:"cover_i"`
}

type workDetailsResponse struct {
	Key              string          `json:"key"`
	Title            string          `json:"title"`
	Description      json.RawMessage `json:"description"`
	FirstPublishDate string          `json:"first_publish_date"`
	Covers           []int64         `json:"covers"`
}

type editionsResponse struct {
	Entries []editionResponse `json:"entries"`
}

type editionResponse struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	Languages []struct {
		Key string `json:"key"`
	} `json:"languages"`
	PublishDate string  `json:"publish_date"`
	Covers      []int64 `json:"covers"`
}

// RequestLimiter spaces outgoing requests and can be canceled while waiting.
type RequestLimiter interface {
	Wait(context.Context) error
}

type rateLimiter struct {
	gate     chan struct{}
	interval time.Duration
	next     time.Time
	now      func() time.Time
}

func newRateLimiter(interval time.Duration) *rateLimiter {
	return &rateLimiter{gate: make(chan struct{}, 1), interval: interval, now: time.Now}
}

func (limiter *rateLimiter) Wait(ctx context.Context) error {
	select {
	case limiter.gate <- struct{}{}:
		defer func() { <-limiter.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if delay := limiter.next.Sub(limiter.now()); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	limiter.next = limiter.now().Add(limiter.interval)
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
	if config.Limiter == nil {
		config.Limiter = processRateLimiter
	}
	metadataLanguage := normalizePreferredLanguage(config.MetadataLanguage)
	return &SearchSource{
		endpoint: endpoint, httpClient: &client, requestTimeout: config.RequestTimeout, limiter: config.Limiter,
		metadataLanguage: metadataLanguage, knownEditions: make(map[string]domain.RepresentativeEdition),
	}
}

func (*SearchSource) Name() string { return "openlibrary" }

// SearchCacheIdentity separates normalized results whose representative
// editions were selected using different metadata-language preferences.
func (source *SearchSource) SearchCacheIdentity() string {
	language := source.preferredMetadataLanguage()
	if language == "" {
		language = "original"
	}
	return "metadata-language-" + language
}

// SetMetadataLanguage changes only the preferred representative-edition
// language; it never filters Search results by language.
func (source *SearchSource) SetMetadataLanguage(language string) {
	preferredLanguage := normalizePreferredLanguage(language)
	source.metadataLanguageMu.Lock()
	defer source.metadataLanguageMu.Unlock()
	if source.metadataLanguage == preferredLanguage {
		return
	}
	source.metadataLanguage = preferredLanguage
	source.knownEditionMu.Lock()
	source.knownEditions = make(map[string]domain.RepresentativeEdition)
	source.knownEditionOrder = nil
	source.knownEditionMu.Unlock()
}

func (source *SearchSource) preferredMetadataLanguage() string {
	source.metadataLanguageMu.RLock()
	defer source.metadataLanguageMu.RUnlock()
	return source.metadataLanguage
}

func (source *SearchSource) Search(ctx context.Context, query string, limit int) ([]domain.MetadataSearchResult, error) {
	if err := providers.ValidateSearchRequest(query, limit); err != nil {
		return nil, ErrInvalidQuery
	}
	requestCtx, cancel := context.WithTimeout(ctx, source.requestTimeout)
	defer cancel()
	if err := source.limiter.Wait(requestCtx); err != nil {
		return nil, err
	}
	preferredLanguage := source.preferredMetadataLanguage()
	target, err := searchURL(source.endpoint, strings.TrimSpace(query), limit)
	if err != nil {
		return nil, ErrSearchUnavailable
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
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, ErrSearchThrottled
	default:
		return nil, ErrSearchUnavailable
	}
	body, err := readBounded(response.Body, MaxResponseBodyBytes)
	if err != nil {
		if requestCtx.Err() != nil {
			return nil, requestCtx.Err()
		}
		return nil, err
	}
	var payload searchResponse
	if err := json.Unmarshal(body, &payload); err != nil || payload.Docs == nil {
		return nil, ErrMalformedResponse
	}
	results := make([]domain.MetadataSearchResult, 0, min(limit, len(payload.Docs)))
	for _, document := range payload.Docs {
		if len(results) >= limit {
			break
		}
		match := workKeyPattern.FindStringSubmatch(strings.TrimSpace(document.Key))
		title := strings.TrimSpace(document.Title)
		if len(match) != 2 || title == "" {
			continue
		}
		creators := make([]string, 0, len(document.AuthorNames))
		for _, creator := range document.AuthorNames {
			if creator = strings.TrimSpace(creator); creator != "" {
				creators = append(creators, creator)
			}
		}
		externalID := match[1]
		result := domain.MetadataSearchResult{
			Provider: "openlibrary", ExternalID: externalID, Title: title, Creators: creators,
			MediaType: "book", Year: document.FirstPublishYear, ReleaseYear: document.FirstPublishYear,
			Medium: domain.MediumLiterature, WorkType: "book", SourceURL: "https://openlibrary.org/works/" + externalID,
		}
		edition := representativeSearchEdition(document.Editions.Docs, preferredLanguage)
		if edition != nil {
			result.RepresentativeEdition = edition
			source.rememberEditionForLanguage(externalID, *edition, preferredLanguage)
		}
		coverID := document.CoverID
		if edition != nil && edition.CoverReference != "" {
			result.CoverReference = edition.CoverReference
			result.ArtworkURL = edition.CoverReference
		} else if coverID > 0 {
			cover := coverURL(coverID, "M")
			result.ArtworkURL = cover
			result.CoverReference = cover
		}
		result.SourceRank = len(results) + 1
		results = append(results, result)
	}
	return results, nil
}

func searchURL(endpoint, query string, limit int) (string, error) {
	target, err := url.Parse(endpoint)
	if err != nil || target.Host == "" || (target.Scheme != "https" && target.Scheme != "http") {
		return "", errors.New("invalid Open Library endpoint")
	}
	if target.Path == "" || target.Path == "/" {
		target.Path = "/search.json"
	}
	values := target.Query()
	values.Set("q", query)
	values.Set("fields", "key,title,author_name,first_publish_year,cover_i,editions.key,editions.title,editions.language,editions.publish_date,editions.cover_i")
	values.Set("limit", strconv.Itoa(limit))
	target.RawQuery = values.Encode()
	return target.String(), nil
}

func (source *SearchSource) GetWorkDetails(ctx context.Context, provider, externalID string) (domain.WorkDetails, error) {
	if provider != "openlibrary" {
		return domain.WorkDetails{}, ErrUnsupportedProvider
	}
	if len(externalID) > 32 || !workIDPattern.MatchString(externalID) {
		return domain.WorkDetails{}, ErrInvalidExternalID
	}
	requestCtx, cancel := context.WithTimeout(ctx, source.requestTimeout)
	defer cancel()
	body, err := source.getDetailsBody(requestCtx, "/works/"+externalID+".json", "")
	if err != nil {
		return domain.WorkDetails{}, err
	}
	var work workDetailsResponse
	if err := json.Unmarshal(body, &work); err != nil || work.Key != "/works/"+externalID || cleanOpenLibraryText(work.Title) == "" {
		return domain.WorkDetails{}, ErrDetailsMalformedResponse
	}
	workTitle := cleanOpenLibraryText(work.Title)
	preferredLanguage := source.preferredMetadataLanguage()
	edition, known := source.knownEdition(externalID)
	if needsMetadataEditionLookup(known, edition, preferredLanguage) {
		edition, err = source.lookupRepresentativeEdition(requestCtx, externalID, preferredLanguage)
		if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || requestCtx.Err() != nil) {
			if requestCtx.Err() != nil {
				return domain.WorkDetails{}, requestCtx.Err()
			}
			return domain.WorkDetails{}, err
		}
	}
	firstPublishDate := cleanOpenLibraryText(work.FirstPublishDate)
	details := domain.WorkDetails{
		Provider: "openlibrary", ExternalID: externalID, Title: workTitle,
		Summary:   cleanOpenLibraryText(descriptionValue(work.Description)),
		SourceURL: "https://openlibrary.org/works/" + externalID,
		Medium:    domain.MediumLiterature, WorkType: "book",
		PlatformCandidates: []domain.PlatformCandidate{},
	}
	details.ReleaseYear = publicationYear(firstPublishDate)
	if edition != nil {
		if edition.CoverReference != "" {
			edition.CoverReference = largeCoverURL(edition.CoverReference)
		}
		details.RepresentativeEdition = edition
		if edition.CoverReference != "" {
			details.CoverReference = edition.CoverReference
		}
	}
	if len(work.Covers) > 0 && work.Covers[0] > 0 {
		workCover := coverURL(work.Covers[0], "L")
		if details.CoverReference == "" {
			details.CoverReference = workCover
		} else if details.CoverReference != workCover {
			details.FallbackCoverReference = workCover
		}
	}
	return details, nil
}

func needsMetadataEditionLookup(known bool, edition *domain.RepresentativeEdition, preferredLanguage string) bool {
	return !known || (preferredLanguage != "" && (edition == nil || edition.Language != preferredLanguage))
}

func (source *SearchSource) lookupRepresentativeEdition(ctx context.Context, workID, preferredLanguage string) (*domain.RepresentativeEdition, error) {
	body, err := source.getDetailsBody(ctx, "/works/"+workID+"/editions.json", "limit="+strconv.Itoa(maxRepresentativeEditions))
	if err != nil {
		return nil, err
	}
	var response editionsResponse
	if err := json.Unmarshal(body, &response); err != nil || response.Entries == nil {
		return nil, ErrDetailsMalformedResponse
	}
	var fallback *domain.RepresentativeEdition
	for _, entry := range response.Entries {
		if edition := normalizeEdition(entry); edition != nil {
			if preferredLanguage != "" && edition.Language == preferredLanguage {
				source.rememberEditionForLanguage(workID, *edition, preferredLanguage)
				return edition, nil
			}
			if fallback == nil {
				fallback = edition
			}
		}
	}
	if fallback != nil {
		source.rememberEditionForLanguage(workID, *fallback, preferredLanguage)
	}
	return fallback, nil
}

func (source *SearchSource) getDetailsBody(ctx context.Context, requestPath, rawQuery string) ([]byte, error) {
	if err := source.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	target, err := url.Parse(source.endpoint)
	if err != nil || target.Host == "" || (target.Scheme != "https" && target.Scheme != "http") {
		return nil, ErrDetailsUnavailable
	}
	target.Path = requestPath
	target.RawPath = ""
	target.RawQuery = rawQuery
	target.Fragment = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, ErrDetailsUnavailable
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)
	response, err := source.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrDetailsUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrWorkNotFound
	case http.StatusTooManyRequests:
		return nil, ErrDetailsRateLimited
	default:
		return nil, ErrDetailsUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBodyBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrDetailsUnavailable
	}
	if int64(len(body)) > MaxResponseBodyBytes {
		return nil, ErrDetailsResponseTooLarge
	}
	return body, nil
}

func representativeSearchEdition(candidates []searchEdition, preferredLanguage string) *domain.RepresentativeEdition {
	var fallback *domain.RepresentativeEdition
	for _, candidate := range candidates {
		edition := normalizeSearchEdition(candidate)
		if edition == nil {
			continue
		}
		if edition.Language == preferredLanguage {
			return edition
		}
		if fallback == nil {
			fallback = edition
		}
	}
	return fallback
}

func normalizeSearchEdition(source searchEdition) *domain.RepresentativeEdition {
	match := editionKeyPattern.FindStringSubmatch(strings.TrimSpace(source.Key))
	if len(match) != 2 {
		return nil
	}
	edition := &domain.RepresentativeEdition{
		ExternalID: match[1], Title: cleanOpenLibraryText(source.Title),
		Language: firstLanguage(source.Languages), PublicationDate: cleanOpenLibraryText(source.PublishDate),
	}
	if source.CoverID > 0 {
		edition.CoverReference = coverURL(source.CoverID, "M")
	}
	return edition
}

func normalizeEdition(source editionResponse) *domain.RepresentativeEdition {
	match := editionKeyPattern.FindStringSubmatch(strings.TrimSpace(source.Key))
	if len(match) != 2 {
		return nil
	}
	edition := &domain.RepresentativeEdition{
		ExternalID: match[1], Title: cleanOpenLibraryText(source.Title),
		PublicationDate: cleanOpenLibraryText(source.PublishDate),
	}
	if len(source.Languages) > 0 {
		edition.Language = normalizeLanguage(source.Languages[0].Key)
	}
	if len(source.Covers) > 0 && source.Covers[0] > 0 {
		edition.CoverReference = coverURL(source.Covers[0], "M")
	}
	return edition
}

func (source *SearchSource) rememberEdition(workID string, edition domain.RepresentativeEdition) {
	source.knownEditionMu.Lock()
	defer source.knownEditionMu.Unlock()
	if source.knownEditions == nil {
		source.knownEditions = make(map[string]domain.RepresentativeEdition)
	}
	if _, exists := source.knownEditions[workID]; !exists {
		if len(source.knownEditions) >= maxKnownEditions && len(source.knownEditionOrder) > 0 {
			delete(source.knownEditions, source.knownEditionOrder[0])
			source.knownEditionOrder = source.knownEditionOrder[1:]
		}
		source.knownEditionOrder = append(source.knownEditionOrder, workID)
	}
	source.knownEditions[workID] = edition
}

func (source *SearchSource) rememberEditionForLanguage(workID string, edition domain.RepresentativeEdition, language string) {
	source.metadataLanguageMu.RLock()
	defer source.metadataLanguageMu.RUnlock()
	if source.metadataLanguage != language {
		return
	}
	source.rememberEdition(workID, edition)
}

func (source *SearchSource) knownEdition(workID string) (*domain.RepresentativeEdition, bool) {
	source.knownEditionMu.Lock()
	defer source.knownEditionMu.Unlock()
	edition, exists := source.knownEditions[workID]
	if !exists {
		return nil, false
	}
	copy := edition
	return &copy, true
}

func coverURL(id int64, size string) string {
	if id <= 0 || size != "M" && size != "L" {
		return ""
	}
	return "https://covers.openlibrary.org/b/id/" + strconv.FormatInt(id, 10) + "-" + size + ".jpg"
}

func largeCoverURL(reference string) string {
	const prefix = "https://covers.openlibrary.org/b/id/"
	if !strings.HasPrefix(reference, prefix) || !strings.HasSuffix(reference, "-M.jpg") {
		return ""
	}
	return strings.TrimSuffix(reference, "-M.jpg") + "-L.jpg"
}

func normalizePreferredLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	switch language {
	case "original":
		return ""
	case "en", "es":
		return language
	default:
		return DefaultMetadataLanguage
	}
}

func firstLanguage(candidates []string) string {
	for _, candidate := range candidates {
		if language := normalizeLanguage(candidate); language != "" {
			return language
		}
	}
	return ""
}

func normalizeLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	if slash := strings.LastIndexByte(language, '/'); slash >= 0 {
		language = language[slash+1:]
	}
	if len(language) == 2 && language[0] >= 'a' && language[0] <= 'z' && language[1] >= 'a' && language[1] <= 'z' {
		return language
	}
	return map[string]string{
		"eng": "en", "spa": "es", "fra": "fr", "fre": "fr", "deu": "de", "ger": "de",
		"ita": "it", "por": "pt", "jpn": "ja", "zho": "zh", "chi": "zh", "rus": "ru",
		"ara": "ar", "hin": "hi", "kor": "ko", "nld": "nl", "dut": "nl", "pol": "pl",
		"swe": "sv", "nor": "no", "dan": "da", "fin": "fi", "heb": "he", "tur": "tr",
	}[language]
}

func descriptionValue(description json.RawMessage) string {
	if len(description) == 0 || string(description) == "null" {
		return ""
	}
	var value string
	if json.Unmarshal(description, &value) == nil {
		return value
	}
	var wrapped struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(description, &wrapped) == nil {
		return wrapped.Value
	}
	return ""
}

func publicationYear(publication string) int {
	if len(publication) < 4 {
		return 0
	}
	year, err := strconv.Atoi(publication[:4])
	if err != nil || year < 1 {
		return 0
	}
	return year
}

func cleanOpenLibraryText(value string) string {
	value = html.UnescapeString(value)
	var plain strings.Builder
	for index := 0; index < len(value); {
		if hasCaseInsensitivePrefix(value[index:], "<script") || hasCaseInsensitivePrefix(value[index:], "<style") {
			name := "script"
			if hasCaseInsensitivePrefix(value[index:], "<style") {
				name = "style"
			}
			openingEnd := strings.IndexByte(value[index:], '>')
			if openingEnd < 0 {
				break
			}
			closing := indexCaseInsensitive(value[index+openingEnd+1:], "</"+name)
			if closing < 0 {
				break
			}
			closingStart := index + openingEnd + 1 + closing
			closingEnd := strings.IndexByte(value[closingStart:], '>')
			if closingEnd < 0 {
				break
			}
			index = closingStart + closingEnd + 1
			continue
		}
		if value[index] == '<' {
			if end := strings.IndexByte(value[index:], '>'); end >= 0 {
				index += end + 1
				continue
			}
			break
		}
		runeValue := rune(value[index])
		if runeValue < utf8.RuneSelf {
			if !unicode.IsControl(runeValue) || unicode.IsSpace(runeValue) {
				plain.WriteByte(value[index])
			}
			index++
			continue
		}
		_, size := utf8.DecodeRuneInString(value[index:])
		if size == 0 {
			break
		}
		plain.WriteString(value[index : index+size])
		index += size
	}
	text := strings.Join(strings.Fields(plain.String()), " ")
	if runes := []rune(text); len(runes) > maxNormalizedText {
		text = string(runes[:maxNormalizedText])
	}
	return text
}

func hasCaseInsensitivePrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}

func indexCaseInsensitive(value, target string) int {
	for index := 0; index+len(target) <= len(value); index++ {
		if strings.EqualFold(value[index:index+len(target)], target) {
			return index
		}
	}
	return -1
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
