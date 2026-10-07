// Package igdb implements Lernae's IGDB metadata-search capability.
package igdb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"lernae/internal/config"
	"lernae/internal/domain"
	"lernae/internal/providers"
)

const (
	defaultAuthURL        = "https://id.twitch.tv/oauth2/token"
	defaultAPIBaseURL     = "https://api.igdb.com/v4"
	defaultRequestTimeout = 8 * time.Second
	apiRequestInterval    = 250 * time.Millisecond
	maxOpenAPIRequests    = 8
	maxQueryBytes         = 128
	maxResponseBodyBytes  = 1 << 20
	maxAuthBodyBytes      = 64 << 10
	maxSearchResults      = 10
	maxWorkDetailChildren = 500
	tokenRefreshSkew      = 30 * time.Second
	maxTokenLifetime      = 365 * 24 * time.Hour
)

var (
	ErrConfiguration       = errors.New("IGDB provider configuration is incomplete or invalid")
	ErrInvalidQuery        = errors.New("IGDB search query is invalid")
	ErrAuthentication      = errors.New("IGDB authentication failed")
	ErrUnauthorized        = errors.New("IGDB rejected the request authorization")
	ErrRateLimited         = errors.New("IGDB request rate limit was reached")
	ErrProviderUnavailable = errors.New("IGDB provider is temporarily unavailable")
	ErrProviderRequest     = errors.New("IGDB rejected the request")
	ErrMalformedResponse   = errors.New("IGDB returned an invalid response")
	ErrResponseTooLarge    = errors.New("IGDB response exceeded the configured size limit")
	ErrNetwork             = errors.New("IGDB request failed due to a network error")
	ErrTimeout             = errors.New("IGDB request timed out")
	ErrCanceled            = errors.New("IGDB request was canceled")
	ErrUnsupportedProvider = errors.New("IGDB cannot load details for the requested provider")
	ErrInvalidExternalID   = errors.New("IGDB external work ID is invalid")
	ErrWorkNotFound        = errors.New("IGDB work was not found")
)

// ClientConfig supplies the credentials and test-injectable HTTP endpoints
// used by an IGDBMetadataSource. Empty endpoints select the documented public
// Twitch and IGDB endpoints.
type ClientConfig struct {
	Credentials    config.IGDBCredentials
	AuthURL        string
	APIBaseURL     string
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	Now            func() time.Time
}

// IGDBMetadataSource adapts IGDB game search into provider-neutral domain
// results. Its OAuth token and process-local request limits never leave memory.
type IGDBMetadataSource struct {
	credentials    config.IGDBCredentials
	authURL        string
	apiBaseURL     string
	httpClient     *http.Client
	requestTimeout time.Duration
	now            func() time.Time

	tokenMu        sync.Mutex
	token          string
	tokenRefreshAt time.Time
	tokenCall      *tokenCall
}

type tokenCall struct {
	done  chan struct{}
	token string
	err   error
}

type apiLimiter struct {
	slots  chan struct{}
	mu     sync.Mutex
	nextAt time.Time
}

// processAPILimiter is shared across source instances so constructing multiple
// adapters cannot exceed IGDB's per-process request allowance.
var processAPILimiter = apiLimiter{slots: make(chan struct{}, maxOpenAPIRequests)}

type oauthTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// IGDB-only response types are intentionally kept in this package. The domain
// receives only MetadataSearchResult and Platform values.
type gameResponse struct {
	ID               int64               `json:"id"`
	Name             string              `json:"name"`
	FirstReleaseDate *int64              `json:"first_release_date"`
	Summary          string              `json:"summary"`
	Cover            *coverResponse      `json:"cover"`
	Platforms        []platformResponse  `json:"platforms"`
	ParentGame       *parentGameResponse `json:"parent_game"`
}

type parentGameResponse struct {
	ID int64 `json:"id"`
}

type coverResponse struct {
	ImageID string `json:"image_id"`
}

type platformResponse struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

var _ providers.MetadataSource = (*IGDBMetadataSource)(nil)
var _ providers.WorkDetailsSource = (*IGDBMetadataSource)(nil)

func (*IGDBMetadataSource) String() string {
	return "IGDBMetadataSource{configuration:<redacted>}"
}

func (source *IGDBMetadataSource) GoString() string {
	return source.String()
}

func New(settings ClientConfig) *IGDBMetadataSource {
	if settings.AuthURL == "" {
		settings.AuthURL = defaultAuthURL
	}
	if settings.APIBaseURL == "" {
		settings.APIBaseURL = defaultAPIBaseURL
	}
	if settings.HTTPClient == nil {
		settings.HTTPClient = &http.Client{}
	}
	client := *settings.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if settings.RequestTimeout <= 0 {
		settings.RequestTimeout = defaultRequestTimeout
	}
	if settings.Now == nil {
		settings.Now = time.Now
	}
	return &IGDBMetadataSource{
		credentials:    settings.Credentials,
		authURL:        settings.AuthURL,
		apiBaseURL:     settings.APIBaseURL,
		httpClient:     &client,
		requestTimeout: settings.RequestTimeout,
		now:            settings.Now,
	}
}

func (source *IGDBMetadataSource) SearchWorks(ctx context.Context, query string) ([]domain.MetadataSearchResult, error) {
	if err := source.validateConfiguration(); err != nil {
		return nil, err
	}
	statement, err := gameSearchStatement(query)
	if err != nil {
		return nil, err
	}
	body, err := source.executeGamesQuery(ctx, statement)
	if err != nil {
		return nil, err
	}
	return normalizeGames(body)
}

func (source *IGDBMetadataSource) GetWorkDetails(ctx context.Context, provider, externalID string) (domain.WorkDetails, error) {
	if provider != "igdb" {
		return domain.WorkDetails{}, ErrUnsupportedProvider
	}
	requestedID, err := parseExternalID(externalID)
	if err != nil {
		return domain.WorkDetails{}, err
	}
	if err := source.validateConfiguration(); err != nil {
		return domain.WorkDetails{}, err
	}

	selected, err := source.queryGames(ctx, gameDetailsStatement(requestedID))
	if err != nil {
		return domain.WorkDetails{}, err
	}
	if len(selected) != 1 || selected[0].ID != requestedID {
		return domain.WorkDetails{}, ErrWorkNotFound
	}
	canonical := selected[0]
	if canonical.ParentGame != nil {
		if canonical.ParentGame.ID <= 0 || canonical.ParentGame.ID == canonical.ID {
			return domain.WorkDetails{}, ErrMalformedResponse
		}
		parents, err := source.queryGames(ctx, gameDetailsStatement(canonical.ParentGame.ID))
		if err != nil {
			return domain.WorkDetails{}, err
		}
		if len(parents) != 1 || parents[0].ID != canonical.ParentGame.ID {
			return domain.WorkDetails{}, ErrMalformedResponse
		}
		canonical = parents[0]
	}
	if canonical.ID <= 0 || strings.TrimSpace(canonical.Name) == "" {
		return domain.WorkDetails{}, ErrMalformedResponse
	}

	children, err := source.queryGames(ctx, childGamesStatement(canonical.ID))
	if err != nil {
		return domain.WorkDetails{}, err
	}
	if len(children) > maxWorkDetailChildren {
		return domain.WorkDetails{}, ErrMalformedResponse
	}
	for _, child := range children {
		if child.ID <= 0 {
			return domain.WorkDetails{}, ErrMalformedResponse
		}
	}
	// The provider response order is not a stable presentation order. Sorting
	// lets the first display name for a deduplicated platform be deterministic.
	sort.Slice(children, func(left, right int) bool { return children[left].ID < children[right].ID })

	details := normalizeWorkDetails(canonical)
	platformIndexes := make(map[string]int)
	addPlatformCandidates(&details.PlatformCandidates, platformIndexes, canonical.Platforms, domain.PlatformProvenance{
		Provider: "igdb", ExternalID: strconv.FormatInt(canonical.ID, 10), Relation: domain.PlatformRelationCanonicalWork,
	})
	for _, child := range children {
		if child.ParentGame == nil || child.ParentGame.ID != canonical.ID {
			continue
		}
		addPlatformCandidates(&details.PlatformCandidates, platformIndexes, child.Platforms, domain.PlatformProvenance{
			Provider: "igdb", ExternalID: strconv.FormatInt(child.ID, 10), Relation: domain.PlatformRelationParentGameChild,
		})
	}
	return details, nil
}

func parseExternalID(externalID string) (int64, error) {
	if externalID == "" || len(externalID) > 19 {
		return 0, ErrInvalidExternalID
	}
	for _, character := range externalID {
		if character < '0' || character > '9' {
			return 0, ErrInvalidExternalID
		}
	}
	id, err := strconv.ParseInt(externalID, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalidExternalID
	}
	return id, nil
}

func gameDetailsStatement(id int64) string {
	return "fields id,name,first_release_date,summary,cover.image_id,platforms.name,platforms.slug,parent_game.id;\n" +
		"where id = " + strconv.FormatInt(id, 10) + ";\nlimit 1;"
}

func childGamesStatement(parentID int64) string {
	return "fields id,parent_game.id,platforms.name,platforms.slug;\n" +
		"where parent_game = " + strconv.FormatInt(parentID, 10) + ";\nlimit " + strconv.Itoa(maxWorkDetailChildren) + ";"
}

func (source *IGDBMetadataSource) queryGames(ctx context.Context, statement string) ([]gameResponse, error) {
	body, err := source.executeGamesQuery(ctx, statement)
	if err != nil {
		return nil, err
	}
	var games []gameResponse
	if err := json.Unmarshal(body, &games); err != nil {
		return nil, ErrMalformedResponse
	}
	return games, nil
}

func (source *IGDBMetadataSource) executeGamesQuery(ctx context.Context, statement string) ([]byte, error) {
	token, err := source.getToken(ctx)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		status, body, err := source.gameRequest(ctx, token, statement)
		if err != nil {
			return nil, err
		}
		if status == http.StatusUnauthorized {
			if attempt == 1 {
				return nil, ErrUnauthorized
			}
			source.invalidateToken(token)
			token, err = source.getToken(ctx)
			if err != nil {
				return nil, err
			}
			continue
		}
		if err := providerStatusError(status); err != nil {
			return nil, err
		}
		return body, nil
	}
	return nil, ErrUnauthorized
}

func normalizeWorkDetails(game gameResponse) domain.WorkDetails {
	details := domain.WorkDetails{
		Provider: "igdb", ExternalID: strconv.FormatInt(game.ID, 10), Title: strings.TrimSpace(game.Name),
		Summary: strings.TrimSpace(game.Summary), Medium: domain.MediumGame, WorkType: "game",
		PlatformCandidates: []domain.PlatformCandidate{},
	}
	if game.FirstReleaseDate != nil {
		releaseDate := time.Unix(*game.FirstReleaseDate, 0).UTC()
		details.ReleaseDate = &releaseDate
		details.ReleaseYear = releaseDate.Year()
	}
	if game.Cover != nil && strings.TrimSpace(game.Cover.ImageID) != "" {
		details.CoverReference = "https://images.igdb.com/igdb/image/upload/t_cover_big/" + url.PathEscape(game.Cover.ImageID) + ".jpg"
	}
	return details
}

func addPlatformCandidates(candidates *[]domain.PlatformCandidate, indexes map[string]int, platforms []platformResponse, provenance domain.PlatformProvenance) {
	for _, item := range platforms {
		platform := domain.Platform{Name: strings.TrimSpace(item.Name), Slug: strings.TrimSpace(item.Slug)}
		if platform.Name == "" && platform.Slug == "" {
			continue
		}
		identity := strings.ToLower(platform.Slug)
		if identity == "" {
			identity = strings.ToLower(platform.Name)
		}
		index, exists := indexes[identity]
		if !exists {
			indexes[identity] = len(*candidates)
			*candidates = append(*candidates, domain.PlatformCandidate{Platform: platform, Provenance: []domain.PlatformProvenance{provenance}})
			continue
		}
		candidate := &(*candidates)[index]
		provenanceExists := false
		for _, existing := range candidate.Provenance {
			if existing == provenance {
				provenanceExists = true
				break
			}
		}
		if !provenanceExists {
			candidate.Provenance = append(candidate.Provenance, provenance)
		}
	}
}

func (source *IGDBMetadataSource) validateConfiguration() error {
	if source == nil || source.credentials.Validate() != nil || !validEndpoint(source.authURL) || !validEndpoint(source.apiBaseURL) {
		return ErrConfiguration
	}
	return nil
}

func validEndpoint(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" &&
		parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func gameSearchStatement(query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > maxQueryBytes || !utf8.ValidString(query) {
		return "", ErrInvalidQuery
	}
	for _, character := range query {
		if unicode.IsControl(character) || unicode.In(character, unicode.Zl, unicode.Zp) || character == '"' || character == '\\' || character == ';' {
			return "", ErrInvalidQuery
		}
	}
	return "fields id,name,first_release_date,summary,cover.image_id,platforms.name,platforms.slug;\n" +
		"where version_parent = null & name != null;\n" +
		"search \"" + query + "\";\n" +
		"limit " + strconv.Itoa(maxSearchResults) + ";", nil
}

func (source *IGDBMetadataSource) getToken(ctx context.Context) (string, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	now := source.now()
	source.tokenMu.Lock()
	if source.token != "" && now.Before(source.tokenRefreshAt) {
		token := source.token
		source.tokenMu.Unlock()
		return token, nil
	}
	if flight := source.tokenCall; flight != nil {
		source.tokenMu.Unlock()
		select {
		case <-flight.done:
			return flight.token, flight.err
		case <-ctx.Done():
			return "", requestContextError(ctx.Err())
		}
	}
	flight := &tokenCall{done: make(chan struct{})}
	source.tokenCall = flight
	source.tokenMu.Unlock()

	token, refreshAt, err := source.requestToken(ctx)
	source.tokenMu.Lock()
	if err == nil {
		source.token = token
		source.tokenRefreshAt = refreshAt
	}
	flight.token, flight.err = token, err
	source.tokenCall = nil
	close(flight.done)
	source.tokenMu.Unlock()
	return token, err
}

func (source *IGDBMetadataSource) requestToken(ctx context.Context) (string, time.Time, error) {
	requestContext, cancel := context.WithTimeout(ctx, source.requestTimeout)
	defer cancel()
	form := url.Values{}
	form.Set("client_id", source.credentials.ClientID)
	form.Set("client_secret", source.credentials.ClientSecret)
	form.Set("grant_type", "client_credentials")
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, source.authURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, ErrConfiguration
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := source.httpClient.Do(request)
	if err != nil {
		return "", time.Time{}, requestError(requestContext, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusTooManyRequests {
			return "", time.Time{}, ErrRateLimited
		}
		if response.StatusCode >= http.StatusInternalServerError {
			return "", time.Time{}, ErrProviderUnavailable
		}
		return "", time.Time{}, ErrAuthentication
	}
	body, err := readBounded(response.Body, maxAuthBodyBytes)
	if err != nil {
		if requestContext.Err() != nil {
			return "", time.Time{}, requestContextError(requestContext.Err())
		}
		return "", time.Time{}, err
	}
	var result oauthTokenResponse
	if err := json.Unmarshal(body, &result); err != nil || strings.TrimSpace(result.AccessToken) == "" || result.ExpiresIn <= 0 || result.ExpiresIn > int64(maxTokenLifetime/time.Second) {
		return "", time.Time{}, ErrMalformedResponse
	}
	expiresIn := time.Duration(result.ExpiresIn) * time.Second
	refreshSkew := tokenRefreshSkew
	if proportionalSkew := expiresIn / 10; proportionalSkew < refreshSkew {
		refreshSkew = proportionalSkew
	}
	if expiresIn <= refreshSkew {
		refreshSkew = 0
	}
	return result.AccessToken, source.now().Add(expiresIn - refreshSkew), nil
}

func (source *IGDBMetadataSource) invalidateToken(token string) {
	source.tokenMu.Lock()
	if source.token == token {
		source.token = ""
		source.tokenRefreshAt = time.Time{}
	}
	source.tokenMu.Unlock()
}

func (source *IGDBMetadataSource) gameRequest(ctx context.Context, token, statement string) (int, []byte, error) {
	if err := source.enterAPILimit(ctx); err != nil {
		return 0, nil, err
	}
	defer source.leaveAPILimit()
	requestContext, cancel := context.WithTimeout(ctx, source.requestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, strings.TrimRight(source.apiBaseURL, "/")+"/games", strings.NewReader(statement))
	if err != nil {
		return 0, nil, ErrConfiguration
	}
	request.Header.Set("Client-ID", source.credentials.ClientID)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "text/plain")
	response, err := source.httpClient.Do(request)
	if err != nil {
		return 0, nil, requestError(requestContext, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, nil, nil
	}
	body, err := readBounded(response.Body, maxResponseBodyBytes)
	if err != nil {
		if requestContext.Err() != nil {
			return 0, nil, requestContextError(requestContext.Err())
		}
		return 0, nil, err
	}
	return response.StatusCode, body, nil
}

func (source *IGDBMetadataSource) enterAPILimit(ctx context.Context) error {
	return processAPILimiter.enter(ctx)
}

func (source *IGDBMetadataSource) leaveAPILimit() {
	processAPILimiter.leave()
}

func (limiter *apiLimiter) enter(ctx context.Context) error {
	select {
	case limiter.slots <- struct{}{}:
	case <-ctx.Done():
		return requestContextError(ctx.Err())
	}
	now := time.Now()
	limiter.mu.Lock()
	startAt := limiter.nextAt
	if startAt.Before(now) {
		startAt = now
	}
	limiter.nextAt = startAt.Add(apiRequestInterval)
	limiter.mu.Unlock()
	if delay := time.Until(startAt); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			<-limiter.slots
			return requestContextError(ctx.Err())
		}
	}
	return nil
}

func (limiter *apiLimiter) leave() {
	<-limiter.slots
}

func providerStatusError(status int) error {
	switch {
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status >= http.StatusInternalServerError:
		return ErrProviderUnavailable
	case status >= http.StatusBadRequest:
		return ErrProviderRequest
	default:
		return nil
	}
}

func normalizeGames(body []byte) ([]domain.MetadataSearchResult, error) {
	var games []gameResponse
	if err := json.Unmarshal(body, &games); err != nil {
		return nil, ErrMalformedResponse
	}
	results := make([]domain.MetadataSearchResult, 0, len(games))
	for _, game := range games {
		if game.ID <= 0 || strings.TrimSpace(game.Name) == "" {
			return nil, ErrMalformedResponse
		}
		result := domain.MetadataSearchResult{
			Provider: "igdb", ExternalID: strconv.FormatInt(game.ID, 10), Title: strings.TrimSpace(game.Name),
			Summary: strings.TrimSpace(game.Summary), Medium: domain.MediumGame, WorkType: "game",
			Platforms: make([]domain.Platform, 0, len(game.Platforms)),
		}
		if game.FirstReleaseDate != nil {
			releaseDate := time.Unix(*game.FirstReleaseDate, 0).UTC()
			result.ReleaseDate = &releaseDate
			result.ReleaseYear = releaseDate.Year()
		}
		if game.Cover != nil && strings.TrimSpace(game.Cover.ImageID) != "" {
			result.CoverReference = "https://images.igdb.com/igdb/image/upload/t_cover_big/" + url.PathEscape(game.Cover.ImageID) + ".jpg"
		}
		seenPlatforms := make(map[domain.Platform]struct{}, len(game.Platforms))
		for _, platform := range game.Platforms {
			platform.Name = strings.TrimSpace(platform.Name)
			platform.Slug = strings.TrimSpace(platform.Slug)
			if platform.Name == "" && platform.Slug == "" {
				continue
			}
			normalized := domain.Platform{Name: platform.Name, Slug: platform.Slug}
			if _, exists := seenPlatforms[normalized]; exists {
				continue
			}
			seenPlatforms[normalized] = struct{}{}
			result.Platforms = append(result.Platforms, normalized)
		}
		results = append(results, result)
	}
	return results, nil
}

func readBounded(body io.Reader, limit int64) ([]byte, error) {
	bounded := io.LimitReader(body, limit+1)
	data, err := io.ReadAll(bounded)
	if err != nil {
		return nil, ErrNetwork
	}
	if int64(len(data)) > limit {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

func requestError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return requestContextError(ctxErr)
	}
	var timeoutError interface{ Timeout() bool }
	if errors.As(err, &timeoutError) && timeoutError.Timeout() {
		return ErrTimeout
	}
	return ErrNetwork
}

func requestContextError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return ErrCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return ErrTimeout
	default:
		return ErrNetwork
	}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return ErrCanceled
	}
	if err := ctx.Err(); err != nil {
		return requestContextError(err)
	}
	return nil
}
