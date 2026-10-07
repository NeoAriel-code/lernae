package wikidata

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lernae/internal/providers"
)

const relationshipEntityFixture = `{"entities":{"Q42":{"id":"Q42","lastrevid":1234,"labels":{"en":{"language":"en","value":"  Example   Series "},"es":{"language":"es","value":"Serie de ejemplo"}},"aliases":{"es":[{"language":"es","value":"  Saga   de ejemplo "},{"language":"es","value":"Saga de ejemplo"}]},"claims":{"P179":[{"mainsnak":{"snaktype":"value","datavalue":{"value":{"entity-type":"item","id":"Q10"}}},"rank":"normal","qualifiers":{"P1545":[{"snaktype":"value","datavalue":{"value":"1b"}}]}}],"P527":[{"mainsnak":{"snaktype":"value","datavalue":{"value":{"entity-type":"item","id":"Q11"}}},"rank":"normal","qualifiers":{"P1545":[{"snaktype":"value","datavalue":{"value":"2.0"}}]}}],"P144":[{"mainsnak":{"snaktype":"value","datavalue":{"value":{"entity-type":"item","id":"Q12"}}},"rank":"normal"}],"P155":[{"mainsnak":{"snaktype":"value","datavalue":{"value":{"entity-type":"item","id":"Q13"}}},"rank":"normal"}],"P999":{"unexpected":"raw-provider-marker"}},"unsupported_private_marker":"raw-provider-marker"}}}`

func TestFetchEntityUsesExactActionAPIAndParsesOnlyAllowlistedClaims(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestCount++
		if request.Method != http.MethodGet || request.URL.Path != "/w/api.php" {
			t.Errorf("request = %s %s, want GET /w/api.php", request.Method, request.URL.Path)
		}
		query := request.URL.Query()
		if query.Get("action") != "wbgetentities" || query.Get("ids") != "Q42" ||
			query.Get("props") != "labels|aliases|claims" || query.Get("formatversion") != "2" ||
			query.Get("languages") != "en|es" {
			t.Errorf("Action API query = %v, want exact QID and bounded localized entity read", query)
		}
		if query.Has("sparql") || query.Has("query") {
			t.Errorf("unexpected arbitrary query parameter in Action API request: %v", query)
		}
		if !strings.Contains(request.Header.Get("User-Agent"), "metadata@example.invalid") {
			t.Errorf("User-Agent = %q, want configured contact", request.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, relationshipEntityFixture)
	}))
	defer server.Close()

	config := ClientConfig{ContactEmail: "metadata@example.invalid"}
	client := newTestClient(t, server, config)
	metadata, err := client.FetchEntity(context.Background(), "Q42", []string{"en", "es"})
	if err != nil {
		t.Fatalf("FetchEntity() error = %v", err)
	}
	if requestCount != 1 {
		t.Fatalf("Action API request count = %d, want one exact-entity request", requestCount)
	}
	if metadata.Provider != "wikidata" || metadata.ExternalID != "Q42" || metadata.EntityRevision != "1234" || metadata.ParserVersion != ParserVersion {
		t.Fatalf("parsed provider identity/version = %#v", metadata)
	}
	if len(metadata.Labels) != 2 || metadata.Labels[0].Value != "Example Series" || metadata.Labels[1].Value != "Serie de ejemplo" {
		t.Fatalf("localized labels were not normalized: %#v", metadata.Labels)
	}
	if len(metadata.Aliases) != 1 || metadata.Aliases[0].Language != "es" || metadata.Aliases[0].Value != "Saga de ejemplo" {
		t.Fatalf("localized aliases were not normalized: %#v", metadata.Aliases)
	}
	if len(metadata.Claims) != 4 {
		t.Fatalf("allowlisted claims = %#v, want P179, P527, P155, and P144 only", metadata.Claims)
	}
	claims := make(map[string]string, len(metadata.Claims))
	for _, claim := range metadata.Claims {
		if claim.SubjectExternalID != "Q42" {
			t.Errorf("claim subject = %q, want exact requested QID Q42", claim.SubjectExternalID)
		}
		claims[claim.Property] = claim.ObjectExternalID
		if claim.Property == "P179" && (claim.Ordinal == nil || *claim.Ordinal != "1b") {
			t.Errorf("P179 ordinal = %v, want source text 1b", claim.Ordinal)
		}
		if claim.Property == "P527" && (claim.Ordinal == nil || *claim.Ordinal != "2.0") {
			t.Errorf("P527 ordinal = %v, want source text 2.0", claim.Ordinal)
		}
	}
	if claims["P179"] != "Q10" || claims["P527"] != "Q11" || claims["P144"] != "Q12" || claims["P155"] != "Q13" {
		t.Fatalf("allowlisted claim targets = %v", claims)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, unsupported := range []string{"P999", "raw-provider-marker", "unsupported_private_marker"} {
		if strings.Contains(string(encoded), unsupported) {
			t.Errorf("normalized metadata contains unsupported/raw response field %q", unsupported)
		}
	}
}

func TestFetchEntityParsesOnlyExactLocalProviderIdentifiers(t *testing.T) {
	const body = `{"entities":{"Q42":{"id":"Q42","lastrevid":1234,"claims":{
		"P8600":[{"mainsnak":{"snaktype":"value","datavalue":{"type":"external-id","value":"333"}},"rank":"normal"}],
		"P648":[{"mainsnak":{"snaktype":"value","datavalue":{"type":"external-id","value":"OL123W"}},"rank":"normal"},{"mainsnak":{"snaktype":"value","datavalue":{"type":"external-id","value":"OL124M"}},"rank":"normal"},{"mainsnak":{"snaktype":"value","datavalue":{"type":"external-id","value":"OL125A"}},"rank":"normal"}],
		"P5794":[{"mainsnak":{"snaktype":"value","datavalue":{"type":"external-id","value":"soulcalibur-ii"}},"rank":"normal","qualifiers":{"P9043":[{"snaktype":"value","datavalue":{"type":"external-id","value":"1565"}}]}}],
		"P9043":[{"mainsnak":{"snaktype":"value","datavalue":{"type":"external-id","value":"9999"}},"rank":"normal"}],
		"P9999":[{"mainsnak":{"datavalue":{"value":"private-raw-value"}},"rank":"normal"}]}}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()

	client := newTestClient(t, server, ClientConfig{ContactEmail: "metadata@example.invalid"})
	metadata, err := client.FetchEntity(context.Background(), "Q42", []string{"en"})
	if err != nil {
		t.Fatalf("FetchEntity() error = %v", err)
	}
	want := []providers.RelationshipIdentifier{
		{Property: "P648", Value: "OL123W"},
		{Property: "P8600", Value: "333"},
		{Property: "P9043", QualifierProperty: "P5794", Value: "1565"},
	}
	if !reflect.DeepEqual(metadata.Identifiers, want) {
		t.Fatalf("parsed identifiers = %#v, want %#v", metadata.Identifiers, want)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"OL124M", "OL125A", "9999", "soulcalibur-ii", "P9999", "private-raw-value"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("normalized metadata leaked non-identity/raw value %q: %s", forbidden, encoded)
		}
	}
}

func TestSearchEntitiesReturnsCandidateEntitiesWithoutAutoFetching(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestCount++
		query := request.URL.Query()
		if query.Get("action") != "wbsearchentities" || query.Get("search") != "Example series" || query.Get("language") != "en" || query.Get("limit") != "2" {
			t.Errorf("candidate search query = %v", query)
		}
		_, _ = io.WriteString(w, `{"search":[{"id":"Q10","label":"Example series","description":"a collection","match":{"language":"en"}},{"id":"Q11","label":"Example series","description":"another possible collection","match":{"language":"en"}}]}`)
	}))
	defer server.Close()

	client := newTestClient(t, server, ClientConfig{ContactEmail: "metadata@example.invalid", MaxSearchResults: 2})
	candidates, err := client.SearchEntities(context.Background(), "Example series", "en", 99)
	if err != nil {
		t.Fatalf("SearchEntities() error = %v", err)
	}
	if len(candidates) != 2 || candidates[0].ID != "Q10" || candidates[1].ID != "Q11" {
		t.Fatalf("search candidates = %#v, want both ambiguous candidates retained", candidates)
	}
	if requestCount != 1 {
		t.Fatalf("candidate search triggered %d requests, want one and no automatic entity fetch", requestCount)
	}
}

func TestWikidataClientRequiresConfiguredContactAndExactQIDs(t *testing.T) {
	if _, err := NewClient(ClientConfig{}); err == nil {
		t.Fatal("NewClient() without contact email succeeded")
	}
	for _, config := range []ClientConfig{
		{ContactEmail: "metadata@example.invalid", MaxConcurrentRequests: 4},
		{ContactEmail: "metadata@example.invalid", MaxSearchResults: 11},
		{ContactEmail: "metadata@example.invalid", MaxResponseBytes: DefaultMaxResponseBytes + 1},
		{ContactEmail: "metadata@example.invalid", RequestTimeout: maxRequestTimeout + time.Second},
	} {
		if _, err := NewClient(config); err == nil {
			t.Errorf("NewClient(%#v) accepted a setting beyond hard bounds", config)
		}
	}
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestCount.Add(1)
		_, _ = io.WriteString(w, relationshipEntityFixture)
	}))
	defer server.Close()
	client := newTestClient(t, server, ClientConfig{ContactEmail: "metadata@example.invalid"})
	if _, err := client.FetchEntity(context.Background(), "not-a-qid", []string{"en"}); err == nil {
		t.Fatal("FetchEntity() accepted a non-QID external identity")
	}
	if requestCount.Load() != 0 {
		t.Fatalf("invalid QID caused %d requests, want none", requestCount.Load())
	}
}

func TestWikidataClientBoundsConcurrencyToThreeRequests(t *testing.T) {
	var active, maximum, requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous; previous = maximum.Load() {
			if maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		_, _ = io.WriteString(w, relationshipEntityFixture)
	}))
	defer server.Close()
	client := newTestClient(t, server, ClientConfig{ContactEmail: "metadata@example.invalid"})
	secondClient := newTestClient(t, server, ClientConfig{ContactEmail: "metadata@example.invalid"})

	const lookupCount = 8
	var wait sync.WaitGroup
	for index := range lookupCount {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			selectedClient := client
			if index%2 != 0 {
				selectedClient = secondClient
			}
			if _, err := selectedClient.FetchEntity(context.Background(), "Q42", []string{"en"}); err != nil {
				t.Errorf("FetchEntity() error = %v", err)
			}
		}(index)
	}
	wait.Wait()
	if requests.Load() != lookupCount {
		t.Fatalf("request count = %d, want one per exact-entity lookup", requests.Load())
	}
	if maximum.Load() > 3 {
		t.Fatalf("maximum concurrent requests = %d, want no more than 3", maximum.Load())
	}
}

func TestWikidataClientBoundsResponseBytesAndTimeout(t *testing.T) {
	t.Run("response bytes", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("x", 128))
		}))
		defer server.Close()
		client := newTestClient(t, server, ClientConfig{ContactEmail: "metadata@example.invalid", MaxResponseBytes: 32})
		if _, err := client.FetchEntity(context.Background(), "Q42", []string{"en"}); err == nil {
			t.Fatal("FetchEntity() accepted an oversized provider response")
		}
	})

	t.Run("request timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			time.Sleep(80 * time.Millisecond)
			_, _ = io.WriteString(w, relationshipEntityFixture)
		}))
		defer server.Close()
		client := newTestClient(t, server, ClientConfig{ContactEmail: "metadata@example.invalid", RequestTimeout: 10 * time.Millisecond})
		if _, err := client.FetchEntity(context.Background(), "Q42", []string{"en"}); err == nil {
			t.Fatal("FetchEntity() ignored configured request timeout")
		}
	})
}

func newTestClient(t *testing.T, server *httptest.Server, config ClientConfig) *Client {
	t.Helper()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := endpointRewriteTransport{t: t, target: serverURL, base: server.Client().Transport}
	config.HTTPClient = &http.Client{Transport: transport}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type endpointRewriteTransport struct {
	t      *testing.T
	target *url.URL
	base   http.RoundTripper
}

func (transport endpointRewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != "www.wikidata.org" || request.URL.Path != "/w/api.php" {
		transport.t.Errorf("client request target = %s, want fixed official Wikidata Action API endpoint", request.URL)
	}
	clone := request.Clone(request.Context())
	rewritten := *request.URL
	rewritten.Scheme = transport.target.Scheme
	rewritten.Host = transport.target.Host
	clone.URL = &rewritten
	clone.Host = transport.target.Host
	return transport.base.RoundTrip(clone)
}

func TestFetchEntityRejectsMoreThanOneResponseEntity(t *testing.T) {
	fixture := strings.TrimSuffix(relationshipEntityFixture, "}}") + `,"Q43":{"id":"Q43","lastrevid":2}}}`
	if !json.Valid([]byte(fixture)) {
		t.Fatal("multiple-entity fixture is invalid JSON")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(w, fixture)
	}))
	defer server.Close()
	client := newTestClient(t, server, ClientConfig{ContactEmail: "metadata@example.invalid"})
	if _, err := client.FetchEntity(context.Background(), "Q42", []string{"en"}); err == nil {
		t.Fatal("FetchEntity() accepted more than one response entity")
	}
}
