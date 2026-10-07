// Package wikidata reads small, exact-identity relationship records through
// Wikidata's official MediaWiki Action API.
package wikidata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"lernae/internal/providers"
)

const (
	ActionAPIEndpoint       = "https://www.wikidata.org/w/api.php"
	ParserVersion           = "wikidata-relationship-v3"
	MaxConcurrentRequests   = 3
	DefaultMaxResponseBytes = 1 << 20
	DefaultMaxSearchResults = 10
	MaxLocalizedLanguages   = 5
	MaxRelationshipClaims   = 256
	maxSearchTextBytes      = 256
	maxRequestTimeout       = 30 * time.Second
)

var processRequestSlots = make(chan struct{}, MaxConcurrentRequests)

var (
	ErrInvalidConfiguration = errors.New("invalid Wikidata client configuration")
	ErrInvalidEntityID      = errors.New("invalid Wikidata entity ID")
	ErrInvalidSearch        = errors.New("invalid Wikidata entity search")
	ErrEntityNotFound       = errors.New("Wikidata entity not found")
	ErrResponseTooLarge     = errors.New("Wikidata response exceeds configured byte limit")
	ErrMalformedResponse    = errors.New("malformed Wikidata Action API response")
	ErrRequestUnavailable   = errors.New("Wikidata Action API request unavailable")
	ErrRateLimited          = errors.New("Wikidata Action API rate limited")
)

type ClientConfig struct {
	HTTPClient            *http.Client
	ContactEmail          string
	RequestTimeout        time.Duration
	MaxResponseBytes      int
	MaxConcurrentRequests int
	MaxSearchResults      int
}

type Client struct {
	httpClient       *http.Client
	contactEmail     string
	requestTimeout   time.Duration
	maxResponseBytes int
	maxSearchResults int
	requestSlots     chan struct{}
}

// EntityCandidate is a search suggestion only. Callers must select an exact
// QID explicitly before requesting stronger claims through FetchEntity.
type EntityCandidate struct {
	ID          string
	Label       string
	Description string
	Language    string
}

func NewClient(config ClientConfig) (*Client, error) {
	contact := strings.TrimSpace(config.ContactEmail)
	parsedContact, err := mail.ParseAddress(contact)
	if err != nil || parsedContact.Address != contact || strings.ContainsAny(contact, "\r\n") {
		return nil, fmt.Errorf("%w: a configured contact email is required", ErrInvalidConfiguration)
	}

	if config.RequestTimeout == 0 {
		config.RequestTimeout = 5 * time.Second
	}
	if config.RequestTimeout < 0 || config.RequestTimeout > maxRequestTimeout {
		return nil, fmt.Errorf("%w: request timeout must be between zero and %s", ErrInvalidConfiguration, maxRequestTimeout)
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if config.MaxResponseBytes < 1 || config.MaxResponseBytes > DefaultMaxResponseBytes {
		return nil, fmt.Errorf("%w: response limit must be between 1 and %d bytes", ErrInvalidConfiguration, DefaultMaxResponseBytes)
	}
	if config.MaxConcurrentRequests == 0 {
		config.MaxConcurrentRequests = MaxConcurrentRequests
	}
	if config.MaxConcurrentRequests < 1 || config.MaxConcurrentRequests > MaxConcurrentRequests {
		return nil, fmt.Errorf("%w: concurrent request limit must be between 1 and %d", ErrInvalidConfiguration, MaxConcurrentRequests)
	}
	if config.MaxSearchResults == 0 {
		config.MaxSearchResults = DefaultMaxSearchResults
	}
	if config.MaxSearchResults < 1 || config.MaxSearchResults > DefaultMaxSearchResults {
		return nil, fmt.Errorf("%w: search result limit must be between 1 and %d", ErrInvalidConfiguration, DefaultMaxSearchResults)
	}

	client := &http.Client{}
	if config.HTTPClient != nil {
		*client = *config.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		httpClient: client, contactEmail: contact, requestTimeout: config.RequestTimeout,
		maxResponseBytes: config.MaxResponseBytes,
		maxSearchResults: config.MaxSearchResults, requestSlots: make(chan struct{}, config.MaxConcurrentRequests),
	}, nil
}

// FetchEntity reads exactly one caller-selected QID. No title search or graph
// traversal is performed as part of this request.
func (client *Client) FetchEntity(ctx context.Context, qid string, languages []string) (providers.RelationshipMetadata, error) {
	if !validEntityID(qid) {
		return providers.RelationshipMetadata{}, ErrInvalidEntityID
	}
	requestedLanguages, err := normalizedLanguages(languages)
	if err != nil {
		return providers.RelationshipMetadata{}, err
	}
	params := url.Values{
		"action":        {"wbgetentities"},
		"format":        {"json"},
		"formatversion": {"2"},
		"ids":           {qid},
		"languages":     {strings.Join(requestedLanguages, "|")},
		"props":         {"labels|aliases|claims"},
	}
	body, err := client.request(ctx, params)
	if err != nil {
		return providers.RelationshipMetadata{}, err
	}
	metadata, err := parseEntityResponse(body, qid, requestedLanguages, time.Now().UTC())
	if err != nil {
		return providers.RelationshipMetadata{}, err
	}
	if err := metadata.Validate(); err != nil {
		return providers.RelationshipMetadata{}, fmt.Errorf("%w: %v", ErrMalformedResponse, err)
	}
	return metadata, nil
}

// SearchEntities returns bounded candidates, never a selected entity identity.
// It makes one wbsearchentities request and does not auto-fetch any candidate.
func (client *Client) SearchEntities(ctx context.Context, query, language string, limit int) ([]EntityCandidate, error) {
	query = strings.TrimSpace(query)
	language = strings.ToLower(strings.TrimSpace(language))
	if query == "" || len(query) > maxSearchTextBytes || !validLanguage(language) || limit < 1 {
		return nil, ErrInvalidSearch
	}
	if limit > client.maxSearchResults {
		limit = client.maxSearchResults
	}
	params := url.Values{
		"action":        {"wbsearchentities"},
		"format":        {"json"},
		"formatversion": {"2"},
		"language":      {language},
		"limit":         {strconv.Itoa(limit)},
		"search":        {query},
	}
	body, err := client.request(ctx, params)
	if err != nil {
		return nil, err
	}
	var response struct {
		Search []struct {
			ID          string `json:"id"`
			Label       string `json:"label"`
			Description string `json:"description"`
			Match       struct {
				Language string `json:"language"`
			} `json:"match"`
		} `json:"search"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, ErrMalformedResponse
	}
	candidates := make([]EntityCandidate, 0, min(limit, len(response.Search)))
	seen := make(map[string]struct{}, len(response.Search))
	for _, item := range response.Search {
		if !validEntityID(item.ID) {
			continue
		}
		if _, exists := seen[item.ID]; exists {
			continue
		}
		seen[item.ID] = struct{}{}
		label := normalizeWhitespace(item.Label)
		if len(label) > 512 {
			label = ""
		}
		description := normalizeWhitespace(item.Description)
		if len(description) > 2048 {
			description = ""
		}
		candidates = append(candidates, EntityCandidate{
			ID: item.ID, Label: label, Description: description,
			Language: firstNonEmpty(item.Match.Language, language),
		})
		if len(candidates) == limit {
			break
		}
	}
	return candidates, nil
}

func (client *Client) request(ctx context.Context, params url.Values) ([]byte, error) {
	requestContext, cancel := context.WithTimeout(ctx, client.requestTimeout)
	defer cancel()
	select {
	case processRequestSlots <- struct{}{}:
		defer func() { <-processRequestSlots }()
	case <-requestContext.Done():
		return nil, requestContext.Err()
	}
	select {
	case client.requestSlots <- struct{}{}:
		defer func() { <-client.requestSlots }()
	case <-requestContext.Done():
		return nil, requestContext.Err()
	}
	target := ActionAPIEndpoint + "?" + params.Encode()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, target, nil)
	if err != nil {
		return nil, ErrRequestUnavailable
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Lernae/relationship-metadata (+mailto:"+client.contactEmail+")")
	response, err := client.httpClient.Do(request)
	if err != nil {
		if requestContext.Err() != nil {
			return nil, requestContext.Err()
		}
		return nil, ErrRequestUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, ErrRateLimited
	case http.StatusNotFound:
		return nil, ErrEntityNotFound
	default:
		return nil, ErrRequestUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(client.maxResponseBytes)+1))
	if err != nil {
		if requestContext.Err() != nil {
			return nil, requestContext.Err()
		}
		return nil, ErrRequestUnavailable
	}
	if len(body) > client.maxResponseBytes {
		return nil, ErrResponseTooLarge
	}
	return body, nil
}

type entityResponse struct {
	Entities map[string]entity `json:"entities"`
}

type entity struct {
	ID      string                         `json:"id"`
	Missing bool                           `json:"missing"`
	LastRev int64                          `json:"lastrevid"`
	Labels  map[string]localizedResponse   `json:"labels"`
	Aliases map[string][]localizedResponse `json:"aliases"`
	Claims  map[string]json.RawMessage     `json:"claims"`
}

type localizedResponse struct {
	Language string `json:"language"`
	Value    string `json:"value"`
}

type statement struct {
	MainSnak   snak              `json:"mainsnak"`
	Qualifiers map[string][]snak `json:"qualifiers"`
	Rank       string            `json:"rank"`
}

type snak struct {
	SnakType  string     `json:"snaktype"`
	DataValue *dataValue `json:"datavalue"`
}

type dataValue struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

func parseEntityResponse(body []byte, qid string, languages []string, fetchedAt time.Time) (providers.RelationshipMetadata, error) {
	var response entityResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return providers.RelationshipMetadata{}, ErrMalformedResponse
	}
	if len(response.Entities) != 1 {
		return providers.RelationshipMetadata{}, ErrMalformedResponse
	}
	item, exists := response.Entities[qid]
	if !exists || item.Missing {
		return providers.RelationshipMetadata{}, ErrEntityNotFound
	}
	if item.ID != qid || item.LastRev < 1 {
		return providers.RelationshipMetadata{}, ErrMalformedResponse
	}
	requested := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		requested[strings.ToLower(language)] = struct{}{}
	}
	metadata := providers.RelationshipMetadata{
		Provider: "wikidata", ExternalID: qid, EntityRevision: strconv.FormatInt(item.LastRev, 10),
		ParserVersion: ParserVersion, FetchedAt: fetchedAt.UTC(),
		Labels: localizedLabels(item.Labels, requested), Aliases: localizedAliases(item.Aliases, requested),
	}
	identifiers, err := parseProviderIdentifiers(item.Claims)
	if err != nil || len(identifiers) > providers.MaxRelationshipIdentifiers {
		return providers.RelationshipMetadata{}, ErrMalformedResponse
	}
	metadata.Identifiers = identifiers
	for _, property := range []string{"P179", "P527", "P155", "P144"} {
		rawClaims, exists := item.Claims[property]
		if !exists {
			continue
		}
		var statements []statement
		if err := json.Unmarshal(rawClaims, &statements); err != nil {
			return providers.RelationshipMetadata{}, ErrMalformedResponse
		}
		for _, claim := range statements {
			if claim.Rank != "normal" && claim.Rank != "preferred" {
				continue
			}
			objectID, ok := entityIDFromSnak(claim.MainSnak)
			if !ok {
				continue
			}
			parsed := providers.RelationshipClaim{
				Property: property, SubjectExternalID: qid, ObjectExternalID: objectID,
			}
			if property == "P179" || property == "P527" {
				parsed.Ordinal = ordinalQualifier(claim.Qualifiers["P1545"])
			}
			metadata.Claims = append(metadata.Claims, parsed)
			if len(metadata.Claims) > MaxRelationshipClaims {
				return providers.RelationshipMetadata{}, ErrMalformedResponse
			}
		}
	}
	sort.Slice(metadata.Claims, func(i, j int) bool {
		left, right := metadata.Claims[i], metadata.Claims[j]
		if left.Property != right.Property {
			return left.Property < right.Property
		}
		if left.ObjectExternalID != right.ObjectExternalID {
			return left.ObjectExternalID < right.ObjectExternalID
		}
		return ordinalText(left.Ordinal) < ordinalText(right.Ordinal)
	})
	uniqueClaims := metadata.Claims[:0]
	for _, claim := range metadata.Claims {
		if len(uniqueClaims) == 0 || !sameRelationshipClaim(uniqueClaims[len(uniqueClaims)-1], claim) {
			uniqueClaims = append(uniqueClaims, claim)
		}
	}
	metadata.Claims = uniqueClaims
	return metadata, nil
}

func sameRelationshipClaim(left, right providers.RelationshipClaim) bool {
	return left.Property == right.Property && left.SubjectExternalID == right.SubjectExternalID &&
		left.ObjectExternalID == right.ObjectExternalID && ordinalText(left.Ordinal) == ordinalText(right.Ordinal)
}

func parseProviderIdentifiers(claims map[string]json.RawMessage) ([]providers.RelationshipIdentifier, error) {
	identifiers := make([]providers.RelationshipIdentifier, 0)
	appendIdentifier := func(identifier providers.RelationshipIdentifier) error {
		if _, _, ok := identifier.LocalProviderIdentity(); !ok {
			return nil
		}
		identifiers = append(identifiers, identifier)
		if len(identifiers) > providers.MaxRelationshipIdentifiers {
			return ErrMalformedResponse
		}
		return nil
	}
	for _, property := range []string{"P648", "P8600"} {
		raw, exists := claims[property]
		if !exists {
			continue
		}
		var statements []statement
		if err := json.Unmarshal(raw, &statements); err != nil {
			return nil, ErrMalformedResponse
		}
		for _, claim := range statements {
			if !activeIdentifierRank(claim.Rank) {
				continue
			}
			value, ok := externalIdentifierValue(claim.MainSnak)
			if ok {
				if err := appendIdentifier(providers.RelationshipIdentifier{Property: property, Value: value}); err != nil {
					return nil, err
				}
			}
		}
	}
	var igdbStatements []statement
	if raw, exists := claims["P5794"]; exists {
		if err := json.Unmarshal(raw, &igdbStatements); err != nil {
			return nil, ErrMalformedResponse
		}
		for _, claim := range igdbStatements {
			if !activeIdentifierRank(claim.Rank) {
				continue
			}
			for _, qualifier := range claim.Qualifiers["P9043"] {
				value, ok := externalIdentifierValue(qualifier)
				if ok {
					if err := appendIdentifier(providers.RelationshipIdentifier{
						Property: "P9043", QualifierProperty: "P5794", Value: value,
					}); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	sort.Slice(identifiers, func(i, j int) bool {
		left, right := identifiers[i], identifiers[j]
		if left.Property != right.Property {
			return left.Property < right.Property
		}
		if left.QualifierProperty != right.QualifierProperty {
			return left.QualifierProperty < right.QualifierProperty
		}
		return left.Value < right.Value
	})
	unique := identifiers[:0]
	for _, identifier := range identifiers {
		if len(unique) == 0 || unique[len(unique)-1] != identifier {
			unique = append(unique, identifier)
		}
	}
	return unique, nil
}

func activeIdentifierRank(rank string) bool {
	return rank == "normal" || rank == "preferred"
}

func externalIdentifierValue(value snak) (string, bool) {
	if value.SnakType != "value" || value.DataValue == nil {
		return "", false
	}
	if value.DataValue.Type != "" && value.DataValue.Type != "string" && value.DataValue.Type != "external-id" {
		return "", false
	}
	var parsed string
	if err := json.Unmarshal(value.DataValue.Value, &parsed); err == nil {
		return parsed, true
	}
	// A numeric external-ID may be encoded as a JSON number. Preserve its
	// canonical decimal bytes instead of passing through floating-point JSON.
	return string(value.DataValue.Value), len(value.DataValue.Value) > 0
}

func localizedLabels(values map[string]localizedResponse, requested map[string]struct{}) []providers.RelationshipLocalizedValue {
	result := make([]providers.RelationshipLocalizedValue, 0, len(values))
	for key, value := range values {
		language := strings.ToLower(firstNonEmpty(value.Language, key))
		if _, ok := requested[strings.ToLower(language)]; !ok {
			continue
		}
		text := normalizeWhitespace(value.Value)
		if validLanguage(language) && len(text) > 0 && len(text) <= 512 {
			result = append(result, providers.RelationshipLocalizedValue{Language: language, Value: text})
		}
	}
	return sortLocalizedValues(result)
}

func localizedAliases(values map[string][]localizedResponse, requested map[string]struct{}) []providers.RelationshipLocalizedValue {
	result := make([]providers.RelationshipLocalizedValue, 0)
	for key, aliases := range values {
		for _, alias := range aliases {
			language := strings.ToLower(firstNonEmpty(alias.Language, key))
			if _, ok := requested[strings.ToLower(language)]; !ok {
				continue
			}
			text := normalizeWhitespace(alias.Value)
			if validLanguage(language) && len(text) > 0 && len(text) <= 512 {
				result = append(result, providers.RelationshipLocalizedValue{Language: language, Value: text})
			}
		}
	}
	result = sortLocalizedValues(result)
	if len(result) > 250 {
		result = result[:250]
	}
	return result
}

func sortLocalizedValues(values []providers.RelationshipLocalizedValue) []providers.RelationshipLocalizedValue {
	sort.Slice(values, func(i, j int) bool {
		if values[i].Language != values[j].Language {
			return values[i].Language < values[j].Language
		}
		return values[i].Value < values[j].Value
	})
	if len(values) < 2 {
		return values
	}
	unique := values[:0]
	for _, value := range values {
		if len(unique) == 0 {
			unique = append(unique, value)
			continue
		}
		previous := unique[len(unique)-1]
		if value.Language != previous.Language || !strings.EqualFold(value.Value, previous.Value) {
			unique = append(unique, value)
		}
	}
	return unique
}

func entityIDFromSnak(value snak) (string, bool) {
	if value.SnakType != "value" || value.DataValue == nil {
		return "", false
	}
	var target struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(value.DataValue.Value, &target); err != nil || !validEntityID(target.ID) {
		return "", false
	}
	return target.ID, true
}

func ordinalQualifier(values []snak) *string {
	var ordinal string
	for _, value := range values {
		if value.SnakType != "value" || value.DataValue == nil {
			continue
		}
		var candidate string
		if err := json.Unmarshal(value.DataValue.Value, &candidate); err != nil || strings.TrimSpace(candidate) == "" || len(candidate) > 64 {
			continue
		}
		if ordinal != "" && ordinal != candidate {
			return nil
		}
		ordinal = candidate
	}
	if ordinal == "" {
		return nil
	}
	return &ordinal
}

func normalizedLanguages(languages []string) ([]string, error) {
	if len(languages) == 0 || len(languages) > MaxLocalizedLanguages {
		return nil, ErrInvalidSearch
	}
	result := make([]string, 0, len(languages))
	seen := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		language = strings.ToLower(strings.TrimSpace(language))
		if !validLanguage(language) {
			return nil, ErrInvalidSearch
		}
		key := strings.ToLower(language)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, language)
	}
	if len(result) == 0 {
		return nil, ErrInvalidSearch
	}
	sort.Strings(result)
	return result, nil
}

func validLanguage(language string) bool {
	if len(language) < 2 || len(language) > 35 || strings.TrimSpace(language) != language {
		return false
	}
	for index, part := range strings.Split(language, "-") {
		if len(part) == 0 || len(part) > 8 || (index == 0 && len(part) < 2) {
			return false
		}
		for _, character := range part {
			isLetter := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
			isDigit := character >= '0' && character <= '9'
			if !isLetter && !(index > 0 && isDigit) {
				return false
			}
		}
	}
	return true
}

func validEntityID(value string) bool {
	if len(value) < 2 || len(value) > 20 || value[0] != 'Q' || value[1] == '0' {
		return false
	}
	for _, character := range value[1:] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func normalizeWhitespace(value string) string { return strings.Join(strings.Fields(value), " ") }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func ordinalText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
