package openlibrary

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkDetailsReusesKnownSearchEditionAndPreservesWorkIdentity(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case "/search.json":
			if request.URL.Query().Has("lang") {
				t.Errorf("Search query has language filter %q; language preference must not filter Works", request.URL.Query().Get("lang"))
			}
			_, _ = io.WriteString(w, `{"docs":[{"key":"/works/OL123W","title":"A Work","cover_i":12,"editions":{"docs":[{"key":"/books/OL321M","title":"Spanish edition","language":["spa"],"publish_date":"1991","cover_i":99}]}}]}`)
		case "/works/OL123W.json":
			_, _ = io.WriteString(w, `{"key":"/works/OL123W","title":"A Work","description":{"type":"/type/text","value":"A safe description."},"first_publish_date":"1988","covers":[12]}`)
		default:
			t.Errorf("unexpected request path %q; known Search edition should be reused", request.URL.Path)
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}, MetadataLanguage: "es"})
	if _, err := source.Search(context.Background(), "A Work", 1); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	details, err := source.GetWorkDetails(context.Background(), "openlibrary", "OL123W")
	if err != nil {
		t.Fatalf("GetWorkDetails() error = %v", err)
	}
	if strings.Join(paths, ",") != "/search.json,/works/OL123W.json" {
		t.Fatalf("request paths = %v, want Search followed by only the exact Work lookup", paths)
	}
	if details.Provider != "openlibrary" || details.ExternalID != "OL123W" || details.Title != "A Work" || details.ReleaseYear != 1988 {
		t.Fatalf("normalized Work identity/details = %#v", details)
	}
	edition := details.RepresentativeEdition
	if edition == nil || edition.ExternalID != "OL321M" || edition.Title != "Spanish edition" || edition.Language != "es" ||
		edition.PublicationDate != "1991" || edition.CoverReference != "https://covers.openlibrary.org/b/id/99-L.jpg" {
		t.Fatalf("normalized representative edition = %#v", edition)
	}
	if details.CoverReference != edition.CoverReference || details.SourceURL != "https://openlibrary.org/works/OL123W" || details.Summary != "A safe description." {
		t.Fatalf("normalized Work presentation = %#v", details)
	}
}

func TestMetadataLanguageChangeClearsCachedEditionBeforeOriginalProviderDefault(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case "/search.json":
			if request.URL.Query().Has("lang") {
				t.Errorf("Search request added a language filter: %q", request.URL.Query().Get("lang"))
			}
			_, _ = io.WriteString(w, `{"docs":[{"key":"/works/OL123W","title":"A Work","editions":{"docs":[{"key":"/books/OL321M","title":"English edition","language":["eng"]},{"key":"/books/OL322M","title":"Spanish edition","language":["spa"]}]}}]}`)
		case "/works/OL123W.json":
			_, _ = io.WriteString(w, `{"key":"/works/OL123W","title":"A Work"}`)
		case "/works/OL123W/editions.json":
			_, _ = io.WriteString(w, `{"entries":[{"key":"/books/OL323M","title":"Japanese edition","languages":[{"key":"/languages/jpn"}]},{"key":"/books/OL322M","title":"Spanish edition","languages":[{"key":"/languages/spa"}]}]}`)
		default:
			t.Errorf("unexpected request path %q", request.URL.Path)
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}, MetadataLanguage: "es"})
	if _, err := source.Search(context.Background(), "A Work", 1); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	source.SetMetadataLanguage("original")
	details, err := source.GetWorkDetails(context.Background(), "openlibrary", "OL123W")
	if err != nil {
		t.Fatalf("GetWorkDetails() error = %v", err)
	}
	if details.RepresentativeEdition == nil || details.RepresentativeEdition.ExternalID != "OL323M" {
		t.Fatalf("Original representative edition = %#v, want provider-first Japanese edition", details.RepresentativeEdition)
	}
	if strings.Join(paths, ",") != "/search.json,/works/OL123W.json,/works/OL123W/editions.json" {
		t.Fatalf("request paths = %v, want fresh edition lookup after preference change", paths)
	}
}

func TestWorkDetailsLoadsAtMostOneEditionAndFallsBackToWorkCover(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls++
		switch request.URL.Path {
		case "/works/OL7W.json":
			_, _ = io.WriteString(w, `{"key":"/works/OL7W","title":"A Work","covers":[7]}`)
		case "/works/OL7W/editions.json":
			if request.URL.Query().Get("limit") != "10" {
				t.Errorf("edition lookup query = %v, want one bounded 10-edition lookup", request.URL.Query())
			}
			_, _ = io.WriteString(w, `{"entries":[{"key":"/books/OL8M","title":"One edition","languages":[{"key":"/languages/eng"}],"publish_date":"1990","covers":[]}]}`)
		default:
			t.Errorf("unexpected request path %q", request.URL.Path)
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	details, err := source.GetWorkDetails(context.Background(), "openlibrary", "OL7W")
	if err != nil {
		t.Fatalf("GetWorkDetails() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("request count = %d, want one Work and at most one edition request", calls)
	}
	if details.RepresentativeEdition == nil || details.RepresentativeEdition.ExternalID != "OL8M" || details.RepresentativeEdition.Language != "en" {
		t.Fatalf("edition metadata = %#v", details.RepresentativeEdition)
	}
	if details.CoverReference != "https://covers.openlibrary.org/b/id/7-L.jpg" {
		t.Fatalf("cover reference = %q, want Work cover fallback", details.CoverReference)
	}
}

func TestWorkDetailsPrefersMetadataLanguageWithinBoundedEditionResponse(t *testing.T) {
	var workCalls, editionCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/works/OL21W.json":
			workCalls++
			_, _ = io.WriteString(w, `{"key":"/works/OL21W","title":"A Work"}`)
		case "/works/OL21W/editions.json":
			editionCalls++
			if request.URL.Query().Get("limit") != "10" {
				t.Errorf("edition lookup query = %v, want one bounded 10-edition lookup", request.URL.Query())
			}
			_, _ = io.WriteString(w, `{"entries":[{"key":"/books/OL22M","title":"English edition","languages":[{"key":"/languages/eng"}],"publish_date":"1990"},{"key":"/books/OL23M","title":"Spanish edition","languages":[{"key":"/languages/spa"}],"publish_date":"1992"}]}`)
		default:
			t.Errorf("unexpected request path %q", request.URL.Path)
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}, MetadataLanguage: "es"})
	details, err := source.GetWorkDetails(context.Background(), "openlibrary", "OL21W")
	if err != nil {
		t.Fatalf("GetWorkDetails() error = %v", err)
	}
	if workCalls != 1 || editionCalls != 1 {
		t.Fatalf("request counts = Work %d, editions %d; want one bounded request of each", workCalls, editionCalls)
	}
	if details.ExternalID != "OL21W" {
		t.Fatalf("canonical Work ID = %q, want OL21W", details.ExternalID)
	}
	if edition := details.RepresentativeEdition; edition == nil || edition.ExternalID != "OL23M" || edition.Language != "es" {
		t.Fatalf("representative edition = %#v, want preferred Spanish edition OL23M", edition)
	}
}

func TestWorkDetailsReturnsEditionAndWorkCoversFromOneBoundedLookup(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case "/works/OL31W.json":
			_, _ = io.WriteString(w, `{"key":"/works/OL31W","title":"A Work","covers":[301]}`)
		case "/works/OL31W/editions.json":
			if request.URL.Query().Get("limit") != "10" {
				t.Errorf("edition lookup query = %v, want one bounded 10-edition lookup", request.URL.Query())
			}
			_, _ = io.WriteString(w, `{"entries":[{"key":"/books/OL32M","title":"Representative edition","languages":[{"key":"/languages/eng"}],"covers":[302]}]}`)
		default:
			t.Errorf("unexpected request path %q", request.URL.Path)
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	details, err := source.GetWorkDetails(context.Background(), "openlibrary", "OL31W")
	if err != nil {
		t.Fatalf("GetWorkDetails() error = %v", err)
	}
	if strings.Join(paths, ",") != "/works/OL31W.json,/works/OL31W/editions.json" {
		t.Fatalf("request paths = %v, want one Work request and one bounded edition request", paths)
	}
	if details.ExternalID != "OL31W" || details.RepresentativeEdition == nil || details.RepresentativeEdition.ExternalID != "OL32M" {
		t.Fatalf("canonical Work/representative edition identity = %#v", details)
	}
	if details.CoverReference != "https://covers.openlibrary.org/b/id/302-L.jpg" {
		t.Fatalf("primary edition cover = %q, want safe edition URL", details.CoverReference)
	}
	if details.FallbackCoverReference != "https://covers.openlibrary.org/b/id/301-L.jpg" {
		t.Fatalf("fallback Work cover = %q, want safe Work URL", details.FallbackCoverReference)
	}
}

func TestWorkDetailsRequiresExactOpenLibraryWorkIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		t.Error("invalid identity must not trigger a provider request")
		http.NotFound(w, request)
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	for _, test := range []struct {
		provider string
		id       string
	}{
		{provider: "OpenLibrary", id: "OL1W"},
		{provider: "openlibrary", id: "/works/OL1W"},
		{provider: "openlibrary", id: "https://attacker.test/works/OL1W"},
		{provider: "openlibrary", id: "OL1M"},
	} {
		if _, err := source.GetWorkDetails(context.Background(), test.provider, test.id); err == nil {
			t.Errorf("GetWorkDetails(%q, %q) error = nil, want identity rejection", test.provider, test.id)
		}
	}
}

func TestWorkDetailsRejectsMismatchedProviderWorkKey(t *testing.T) {
	server := jsonServer(`{"key":"/works/OL99W","title":"Different Work"}`)
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	if _, err := source.GetWorkDetails(context.Background(), "openlibrary", "OL7W"); err != ErrDetailsMalformedResponse {
		t.Fatalf("GetWorkDetails() error = %v, want malformed mismatched identity", err)
	}
}
