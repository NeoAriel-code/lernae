package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lernae/internal/domain"
)

const prowlarrTestKey = "PRIVATE_PROWLARR_KEY_123456"

func prowlarrTarget(medium domain.Medium) Target {
	return Target{Edition: domain.Edition{ID: "edition-1", WorkID: "work-1"}, Work: domain.Work{ID: "work-1", Medium: medium, WorkType: "open-ended", Title: "  Exact Existing Title  "}}
}

func newTestProwlarr(t *testing.T, endpoint string) *Prowlarr {
	t.Helper()
	p, err := NewProwlarr(ProwlarrConfig{Enabled: true, BaseURL: endpoint, APIKey: prowlarrTestKey, ReferenceDirectory: filepath.Join(t.TempDir(), "lernae", "acquisition", "prowlarr")})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProwlarrEndpointValidationAndEligibilityWithoutIO(t *testing.T) {
	for _, value := range []string{
		"", "ftp://host", "http://user:password@host", "http://host?key=SECRET", "http://host?", "http://host#", "http://host/#fragment", "http://host/a/../b", "http://host/a//b", "http://host/%2e", "http://host/\\b", "http://host/a/./b", "http://host:99999", "http://host:", " http://host", "http://host\n", "http://host/a//",
	} {
		if _, err := NormalizeProwlarrBaseURL(value); err != ErrProvider {
			t.Errorf("unsafe endpoint accepted or unsafe error")
		}
	}
	for value, want := range map[string]string{
		"HTTP://EXAMPLE.COM:80/base/": "http://example.com/base",
		"https://EXAMPLE.COM:443/":    "https://example.com",
		"http://[::1]:8080/prefix":    "http://[::1]:8080/prefix",
	} {
		got, err := NormalizeProwlarrBaseURL(value)
		if err != nil || got != want {
			t.Errorf("canonical endpoint = %q, want %q", got, want)
		}
	}
	p, err := NewProwlarr(ProwlarrConfig{})
	if err != nil || p.Eligible(prowlarrTarget(domain.MediumGame)) {
		t.Fatal("unconfigured provider eligible")
	}
	p = newTestProwlarr(t, "http://127.0.0.1:1")
	p.config.Enabled = false
	if p.Eligible(prowlarrTarget(domain.MediumGame)) {
		t.Fatal("disabled provider eligible")
	}
	p.config.Enabled = true
	for _, mutate := range []func(*Target){
		func(target *Target) { target.Work.Medium = "unknown" },
		func(target *Target) { target.Work.Title = "   " },
		func(target *Target) { target.Work.Title = strings.Repeat("a", 4097) },
		func(target *Target) { target.Work.Title = "invalid\xff" },
		func(target *Target) { target.Edition.WorkID = "different" },
		func(target *Target) { target.Edition.ID = "../unsafe" },
	} {
		target := prowlarrTarget(domain.MediumVideo)
		mutate(&target)
		if p.Eligible(target) {
			t.Fatal("invalid association/target eligible")
		}
	}
	if _, ok := any(p).(ExecutionResolver); ok {
		t.Fatal("discovery adapter gained resolver authority")
	}
	if _, ok := any(p).(Executor); ok {
		t.Fatal("discovery adapter gained executor authority")
	}
	if got := contentVerificationPolicy(p, 2*time.Second); got.Discovery != 30*time.Second || got.Resolution != 2*time.Second || got.Dispatch != 2*time.Second {
		t.Fatal("bounded discovery policy changed generic defaults")
	}
}

func TestProwlarrExactBasicSearchAllMediaAndDurableLookup(t *testing.T) {
	categories := map[domain.Medium][]string{domain.MediumGame: {"1000", "4050"}, domain.MediumVideo: {"2000", "5000"}, domain.MediumLiterature: {"7000"}, domain.MediumAudio: {"3000"}}
	for medium, want := range categories {
		t.Run(string(medium), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				q := r.URL.Query()
				if r.Method != "GET" || r.URL.Path != "/prefix/api/v1/search" || r.Header.Get("X-Api-Key") != prowlarrTestKey || q.Get("query") != "Exact Existing Title" || q.Get("type") != "search" || q.Get("limit") != "32" || q.Get("offset") != "0" || !reflect.DeepEqual(q["categories"], want) || len(q) != 5 {
					t.Errorf("unexpected protocol request")
				}
				_, _ = w.Write([]byte(`[{"id":0,"indexerId":4,"guid":"PRIVATE_GUID","title":"Real.Release.2026","protocol":"torrent","size":1024},{"indexerId":5,"guid":"SECOND_GUID","title":null,"size":null,"downloadUrl":null,"magnetUrl":null,"categories":null}]`))
			}))
			defer server.Close()
			p := newTestProwlarr(t, server.URL+"/prefix/")
			target := prowlarrTarget(medium)
			if !p.Eligible(target) || p.ID() != "prowlarr" {
				t.Fatal("supported target ineligible")
			}
			options, err := p.Discover(context.Background(), target)
			if err != nil || len(options) != 2 || !validOptions(options) {
				t.Fatalf("discovery failed: %v", err)
			}
			if options[0].ID > options[1].ID {
				t.Fatal("nondeterministic order")
			}
			fresh, err := NewProwlarr(p.config)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range options {
				record, err := fresh.Lookup(context.Background(), o.ExecutionRef)
				if err != nil || record.CandidateID != o.ID || record.Metadata != o.Metadata || record.HasProxyLocator() {
					t.Fatalf("exact lookup failed: %v", err)
				}
				data, _ := json.Marshal(o)
				for _, private := range []string{prowlarrTestKey, server.URL, "PRIVATE_GUID", "SECOND_GUID", o.ExecutionRef} {
					if strings.Contains(string(data), private) {
						t.Fatal("public option leaked private data")
					}
				}
			}
			if calls.Load() != 1 {
				t.Fatal("direct lookup searched remotely")
			}
		})
	}
}

func TestProwlarrCamelCaseQueryPreservesExactTargetAndIdentity(t *testing.T) {
	for _, test := range []struct {
		name  string
		title string
		query string
	}{
		{"confirmed Work title", "SoulCalibur II", "Soul Calibur II"},
		{"already spaced", "  Soul Calibur II  ", "Soul Calibur II"},
		{"normal title", "The Legend of Zelda", "The Legend of Zelda"},
		{"acronyms", "DOOM II HD", "DOOM II HD"},
		{"acronym word boundary", "XMLParser HDRemaster", "XML Parser HD Remaster"},
		{"punctuation and digits", "NieR:Automata-HD 2D", "Nie R:Automata-HD 2D"},
		{"punctuation unchanged", "Soul-Calibur II: HD (2026)", "Soul-Calibur II: HD (2026)"},
		{"Unicode casing", "éClair ÜberSpiel", "é Clair Über Spiel"},
		{"uncased Unicode", "魂刃 II", "魂刃 II"},
		{"combining marks preserved", "Cafe\u0301Noir", "Cafe\u0301Noir"},
		{"maximum title bytes", strings.Repeat("aB", 2048), strings.Repeat("a B", 2048)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				q := r.URL.Query()
				if q.Get("query") != test.query {
					t.Errorf("lexical query differs from expected word boundaries")
				}
				if r.Method != "GET" || r.URL.Path != "/prefix/api/v1/search" || r.Header.Get("X-Api-Key") != prowlarrTestKey || q.Get("type") != "search" || q.Get("limit") != "32" || q.Get("offset") != "0" || !reflect.DeepEqual(q["categories"], []string{"1000", "4050"}) || len(q) != 5 {
					t.Error("search contract changed")
				}
				_, _ = w.Write([]byte(`[{"indexerId":4,"guid":"synthetic-query-guid","title":"Synthetic.Release.II","protocol":"torrent"}]`))
			}))
			defer server.Close()
			p := newTestProwlarr(t, server.URL+"/prefix")
			target := prowlarrTarget(domain.MediumGame)
			target.Work.Title = test.title
			original := target
			options, err := p.Discover(context.Background(), target)
			if err != nil || len(options) != 1 || !validOptions(options) {
				t.Fatalf("query discovery = %v/%d", err, len(options))
			}
			if !reflect.DeepEqual(target, original) {
				t.Fatal("query normalization mutated Work/Edition provenance")
			}
			want := prowlarrRecord{Version: 1, Instance: p.instance, IndexerID: 4, GUID: "synthetic-query-guid", Title: "Synthetic.Release.II", Protocol: ProwlarrProtocolTorrent}
			if options[0].ID != want.candidate() || options[0].ExecutionRef != want.reference() || options[0].Metadata != want.metadata() {
				t.Fatal("query changed exact release identity or presentation")
			}
			record, err := p.Lookup(context.Background(), options[0].ExecutionRef)
			if err != nil || record.CandidateID != options[0].ID || record.Metadata != options[0].Metadata {
				t.Fatal("exact lookup failed")
			}
			if calls.Load() != 1 {
				t.Fatal("query normalization or exact lookup added requests")
			}
		})
	}
}

func TestProwlarrDocumentedProtocolStringsAndPrivateRestart(t *testing.T) {
	// Pinned OpenAPI DownloadProtocol is a string enum, referenced by
	// ReleaseResource.protocol. Missing/null metadata normalizes to unknown.
	for _, test := range []struct {
		name string
		wire string
		want string
	}{
		{"unknown", `,"protocol":"unknown"`, "unknown"},
		{"usenet", `,"protocol":"usenet"`, "usenet"},
		{"torrent", `,"protocol":"torrent"`, "torrent"},
		{"missing", "", "unknown"},
		{"null", `,"protocol":null`, "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `[{"indexerId":1,"guid":"private-guid"` + test.wire + `}]`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			p := newTestProwlarr(t, server.URL)
			options, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumVideo))
			if err != nil || len(options) != 1 {
				t.Fatalf("documented protocol discovery = %v/%d", err, len(options))
			}
			fresh, err := NewProwlarr(p.config)
			if err != nil {
				t.Fatal(err)
			}
			record, err := fresh.Lookup(context.Background(), options[0].ExecutionRef)
			if err != nil || fmt.Sprint(record.Protocol) != test.want {
				t.Fatalf("exact protocol restart = %v, protocol_matches=%t", err, fmt.Sprint(record.Protocol) == test.want)
			}
			data, err := os.ReadFile(filepath.Join(p.config.ReferenceDirectory, options[0].ExecutionRef))
			var stored struct{ Protocol string }
			if err != nil || json.Unmarshal(data, &stored) != nil || stored.Protocol != test.want {
				t.Fatal("private protocol is not the normalized documented string")
			}
		})
	}
}

func TestProwlarrStrictResponsesOptionalMetadataAndSafeFailures(t *testing.T) {
	good := `{"indexerId":1,"guid":"secret-guid"}`
	for _, test := range []struct {
		name  string
		body  string
		want  error
		count int
	}{
		{"empty", `[]`, nil, 0},
		{"optional absent", `[` + good + `]`, nil, 1},
		{"usenet with zero size", `[{"indexerId":1,"guid":"a","title":"Usenet.Release","protocol":"usenet","size":0}]`, nil, 1},
		{"unknown protocol", `[{"indexerId":1,"guid":"a","protocol":"unknown"}]`, nil, 1},
		{"torrent protocol", `[{"indexerId":1,"guid":"a","protocol":"torrent"}]`, nil, 1},
		{"documented nullable", `[{"indexerId":1,"guid":"secret-guid","id":null,"title":null,"size":null,"protocol":null,"downloadUrl":null,"magnetUrl":null,"indexer":null,"categories":null,"language":null,"seeders":null,"leechers":null,"publishDate":null,"rejections":null}]`, nil, 1},
		{"duplicate identical", `[` + good + `,` + good + `]`, nil, 1},
		{"duplicate equivalent optional defaults", `[{"indexerId":1,"guid":"a","title":null,"protocol":null},{"indexerId":1,"guid":"a","title":"","protocol":"unknown"},{"indexerId":1,"guid":"a"}]`, nil, 1},
		{"conflicting protocol duplicate", `[{"indexerId":1,"guid":"a","protocol":"usenet"},{"indexerId":1,"guid":"a","protocol":"torrent"}]`, ErrInvalidResponse, 0},
		{"transient ids ignored", `[{"indexerId":1,"guid":"secret-guid","id":0},{"indexerId":1,"guid":"secret-guid","id":9}]`, nil, 1},
		{"conflicting duplicate", `[` + good + `,{"indexerId":1,"guid":"secret-guid","size":42}]`, ErrInvalidResponse, 0},
		{"malformed", `[`, ErrInvalidResponse, 0},
		{"null array", `null`, ErrInvalidResponse, 0},
		{"object top", `{}`, ErrInvalidResponse, 0},
		{"null item", `[null]`, ErrInvalidResponse, 0},
		{"array item", `[[]]`, ErrInvalidResponse, 0},
		{"trailing JSON", `[] {}`, ErrInvalidResponse, 0},
		{"duplicate keys", `[{"indexerId":1,"guid":"a","guid":"b"}]`, ErrInvalidResponse, 0},
		{"nested duplicate", `[{"indexerId":1,"guid":"a","categories":[{"id":1,"id":2}]}]`, ErrInvalidResponse, 0},
		{"wrong title", `[{"indexerId":1,"guid":"a","title":12}]`, ErrInvalidResponse, 0},
		{"wrong locator", `[{"indexerId":1,"guid":"a","downloadUrl":{}}]`, ErrInvalidResponse, 0},
		{"wrong indexer name", `[{"indexerId":1,"guid":"a","indexer":123}]`, ErrInvalidResponse, 0},
		{"wrong categories", `[{"indexerId":1,"guid":"a","categories":{}}]`, ErrInvalidResponse, 0},
		{"wrong category type", `[{"indexerId":1,"guid":"a","categories":[{"id":"string"}]}]`, ErrInvalidResponse, 0},
		{"optional category shape", `[{"indexerId":1,"guid":"a","categories":[{"id":null,"name":null,"subCategories":null},{}]}]`, nil, 1},
		{"missing guid", `[{"indexerId":1}]`, ErrInvalidResponse, 0},
		{"null guid", `[{"indexerId":1,"guid":null}]`, ErrInvalidResponse, 0},
		{"missing indexer", `[{"guid":"a"}]`, ErrInvalidResponse, 0},
		{"zero indexer", `[{"indexerId":0,"guid":"a"}]`, ErrInvalidResponse, 0},
		{"fractional indexer", `[{"indexerId":1.5,"guid":"a"}]`, ErrInvalidResponse, 0},
		{"string size", `[{"indexerId":1,"guid":"a","size":"12"}]`, ErrInvalidResponse, 0},
		{"overflow size", `[{"indexerId":1,"guid":"a","size":9223372036854775808}]`, ErrInvalidResponse, 0},
		{"negative size", `[{"indexerId":1,"guid":"a","size":-1}]`, ErrInvalidResponse, 0},
		{"bound size", `[{"indexerId":1,"guid":"a","size":1152921504606846977}]`, ErrInvalidResponse, 0},
		{"unknown enum", `[{"indexerId":1,"guid":"a","protocol":"other"}]`, ErrInvalidResponse, 0},
		{"empty enum", `[{"indexerId":1,"guid":"a","protocol":""}]`, ErrInvalidResponse, 0},
		{"case enum", `[{"indexerId":1,"guid":"a","protocol":"Torrent"}]`, ErrInvalidResponse, 0},
		{"numeric zero protocol", `[{"indexerId":1,"guid":"a","protocol":0}]`, ErrInvalidResponse, 0},
		{"numeric one protocol", `[{"indexerId":1,"guid":"a","protocol":1}]`, ErrInvalidResponse, 0},
		{"numeric two protocol", `[{"indexerId":1,"guid":"a","protocol":2}]`, ErrInvalidResponse, 0},
		{"numeric out of range protocol", `[{"indexerId":1,"guid":"a","protocol":3}]`, ErrInvalidResponse, 0},
		{"boolean protocol", `[{"indexerId":1,"guid":"a","protocol":true}]`, ErrInvalidResponse, 0},
		{"case ambiguity", `[{"indexerId":1,"guid":"a","Guid":"b"}]`, ErrInvalidResponse, 0},
		{"oversize", `[` + strings.Repeat(" ", prowlarrMaxResponseBytes) + `]`, ErrInvalidResponse, 0},
		{"current key in guid", `[{"indexerId":1,"guid":"` + prowlarrTestKey + `"}]`, ErrInvalidResponse, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(test.body)) }))
			defer server.Close()
			p := newTestProwlarr(t, server.URL)
			options, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumGame))
			if !errors.Is(err, test.want) || len(options) != test.count || calls.Load() != 1 {
				t.Fatalf("strict response result = %v/%d, calls %d", err, len(options), calls.Load())
			}
			if err == nil && !validOptions(options) {
				t.Fatal("missing optional metadata lost valid candidates")
			}
		})
	}
}

func TestProwlarrHTTPStatusesRedirectsCancellationAndNoRetry(t *testing.T) {
	var leaked atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer foreign.Close()
	for _, status := range []int{401, 403, 429, 500, 502, 503, 301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", foreign.URL+"/PRIVATE")
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(prowlarrTestKey + " RAW_UPSTREAM_ERROR"))
			}))
			defer server.Close()
			p := newTestProwlarr(t, server.URL)
			_, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumGame))
			if err != ErrProvider || calls.Load() != 1 {
				t.Fatal("status leaked error or retried")
			}
		})
	}
	if leaked.Load() != 0 {
		t.Fatal("redirect sent credentials across origin")
	}
	// Same-origin redirects are also refused, including when the route changes.
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "/other")
		w.WriteHeader(302)
	}))
	p := newTestProwlarr(t, server.URL)
	_, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumGame))
	server.Close()
	if err != ErrProvider || calls.Load() != 1 {
		t.Fatal("same-origin redirect followed")
	}
	for _, deadline := range []bool{false, true} {
		started := make(chan struct{})
		joined := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(joined) }))
		p := newTestProwlarr(t, server.URL)
		ctx, cancel := context.WithCancel(context.Background())
		want := error(ErrCancelled)
		if deadline {
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), 40*time.Millisecond)
			want = ErrTimeout
		}
		result := make(chan error, 1)
		go func() { _, err := p.Discover(ctx, prowlarrTarget(domain.MediumGame)); result <- err }()
		<-started
		if !deadline {
			cancel()
		}
		if err := <-result; err != want {
			t.Errorf("context category = %v, want %v", err, want)
		}
		cancel()
		<-joined
		server.Close()
	}
}

func TestProwlarrDeterministicCapDeduplicationAndSnapshots(t *testing.T) {
	var releases []map[string]any
	for i := 0; i < 50; i++ {
		releases = append(releases, map[string]any{"indexerId": 7, "guid": fmt.Sprintf("GUID-%02d", i), "title": "Release", "size": 0})
	}
	body, _ := json.Marshal(releases)
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); defer mu.Unlock(); _, _ = w.Write(body) }))
	defer server.Close()
	p := newTestProwlarr(t, server.URL)
	first, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumGame))
	if err != nil || len(first) != 32 {
		t.Fatal("cap failed")
	}
	for i, j := 0, len(releases)-1; i < j; i, j = i+1, j-1 {
		releases[i], releases[j] = releases[j], releases[i]
	}
	releases = append(releases, releases[0])
	mu.Lock()
	body, _ = json.Marshal(releases)
	mu.Unlock()
	second, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumGame))
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("upstream order or duplicate changed bounded snapshot")
	}
	// Changing meaningful metadata keeps identity but creates a new immutable token.
	old := first[0]
	private, err := p.Lookup(context.Background(), old.ExecutionRef)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	body, _ = json.Marshal([]map[string]any{{"indexerId": private.IndexerID, "guid": private.GUID, "title": "Changed release", "size": 8}})
	mu.Unlock()
	changed, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumGame))
	if err != nil || len(changed) != 1 || changed[0].ID != old.ID || changed[0].ExecutionRef == old.ExecutionRef {
		t.Fatal("snapshot changes not immutable")
	}
	loaded, err := p.Lookup(context.Background(), old.ExecutionRef)
	if err != nil || loaded.Metadata != old.Metadata {
		t.Fatal("old record mutated")
	}
}

func TestProwlarrCrossInstanceConcurrentImmutablePublication(t *testing.T) {
	for _, distinct := range []bool{false, true} {
		name := "identical snapshots"
		if distinct {
			name = "different snapshots same candidate"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				query := r.URL.Query().Get("query")
				if r.Method != "GET" || r.URL.Path != "/api/v1/search" || r.Header.Get("X-Api-Key") != prowlarrTestKey || query != "Exact Existing Title" && query != "Changed Existing Title" {
					t.Error("unexpected concurrent search request")
				}
				title := "Release"
				if query == "Changed Existing Title" {
					title = "Changed Release"
				}
				_ = json.NewEncoder(w).Encode([]map[string]any{{"indexerId": 1, "guid": "private-guid", "title": title, "protocol": "torrent"}})
			}))
			defer server.Close()
			p := newTestProwlarr(t, server.URL)
			// Twelve independent adapter instances share an initially missing
			// private directory: no in-process/store-wide serialization can help.
			providers := make([]*Prowlarr, 12)
			for i := range providers {
				var err error
				providers[i], err = NewProwlarr(p.config)
				if err != nil {
					t.Fatal(err)
				}
			}
			type outcome struct {
				options []Option
				err     error
				changed bool
			}
			results := make(chan outcome, 12)
			start := make(chan struct{})
			for i, provider := range providers {
				go func(i int, provider *Prowlarr) {
					<-start
					target := prowlarrTarget(domain.MediumGame)
					changed := distinct && i%2 == 1
					if changed {
						target.Work.Title = "Changed Existing Title"
					}
					options, err := provider.Discover(context.Background(), target)
					results <- outcome{options, err, changed}
				}(i, provider)
			}
			close(start)
			var outcomes []outcome
			for range providers {
				outcomes = append(outcomes, <-results)
			}
			fresh, err := NewProwlarr(p.config)
			if err != nil {
				t.Fatal(err)
			}
			candidate := ""
			refs := make(map[string]Metadata)
			for _, result := range outcomes {
				if result.err != nil || len(result.options) != 1 {
					t.Fatal("cross-instance publication failed", result.err)
				}
				o := result.options[0]
				if candidate == "" {
					candidate = o.ID
				}
				wantTitle := "Release"
				if result.changed {
					wantTitle = "Changed Release"
				}
				record, lookupErr := fresh.Lookup(context.Background(), o.ExecutionRef)
				data, readErr := os.ReadFile(filepath.Join(p.config.ReferenceDirectory, o.ExecutionRef))
				if lookupErr != nil || readErr != nil || o.ID != candidate || record.CandidateID != candidate || record.Metadata != o.Metadata || o.Metadata.Title != wantTitle || prowlarrHash("reference-v1", data) != o.ExecutionRef {
					t.Fatal("cross-instance publication lost exact immutable identity")
				}
				if previous, ok := refs[o.ExecutionRef]; ok && previous != o.Metadata {
					t.Fatal("different snapshots replaced a shared token")
				}
				refs[o.ExecutionRef] = o.Metadata
			}
			wantRecords := 1
			if distinct {
				wantRecords = 2
			}
			entries, err := os.ReadDir(p.config.ReferenceDirectory)
			if err != nil || len(entries) != wantRecords || len(refs) != wantRecords || calls.Load() != 12 {
				t.Fatal("unexpected request count, immutable count, or partial residue")
			}
		})
	}
}

func TestProwlarrPrivateProxyRecordRotationRestartAndPermissions(t *testing.T) {
	var response []byte
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write(response) }))
	defer server.Close()
	base := server.URL + "/prefix"
	proxy := base + "/9/download?" + url.Values{"apikey": {prowlarrTestKey}, "link": {"AES_Protected-Link"}, "file": {"Real Release"}}.Encode()
	response, _ = json.Marshal([]map[string]any{{"indexerId": 9, "guid": "https://tracker.invalid/PRIVATE_PASSKEY", "title": "Real Release", "protocol": "torrent", "downloadUrl": proxy, "magnetUrl": proxy}})
	p := newTestProwlarr(t, base)
	options, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumGame))
	if err != nil || len(options) != 1 {
		t.Fatal("valid proxy discovery failed", err)
	}
	ref := options[0].ExecutionRef
	recordPath := filepath.Join(p.config.ReferenceDirectory, ref)
	data, err := os.ReadFile(recordPath)
	if err != nil || strings.Contains(string(data), prowlarrTestKey) || strings.Contains(string(data), server.URL) || strings.Contains(string(data), "apikey") {
		t.Fatal("record retained authenticated locator")
	}
	for path, mode := range map[string]os.FileMode{recordPath: 0600, p.config.ReferenceDirectory: 0700, filepath.Dir(p.config.ReferenceDirectory): 0700, filepath.Dir(filepath.Dir(p.config.ReferenceDirectory)): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("secret permissions not enforced")
		}
	}
	config := p.config
	config.APIKey = "ROTATED_PRIVATE_KEY_987654"
	fresh, err := NewProwlarr(config)
	if err != nil {
		t.Fatal(err)
	}
	record, err := fresh.Lookup(context.Background(), ref)
	if err != nil || !record.HasProxyLocator() || record.Download.Route != "/prefix/9/download" || record.Download.Link != "AES_Protected-Link" || record.GUID != "https://tracker.invalid/PRIVATE_PASSKEY" {
		t.Fatal("restart/key rotation lost exact keyless record")
	}
	for _, value := range []any{record, record.Download, p, p.config} {
		encoded, _ := json.Marshal(value)
		for _, text := range []string{string(encoded), fmt.Sprintf("%v", value), fmt.Sprintf("%+v", value), fmt.Sprintf("%#v", value)} {
			for _, secret := range []string{prowlarrTestKey, server.URL, "PRIVATE_PASSKEY", "AES_Protected-Link"} {
				if strings.Contains(text, secret) {
					t.Fatal("trusted private DTO diagnostic spill")
				}
			}
		}
	}
	if calls.Load() != 1 {
		t.Fatal("lookup repeated Search")
	}
	config.BaseURL = server.URL + "/different"
	other, _ := NewProwlarr(config)
	if _, err := other.Lookup(context.Background(), ref); err != ErrUnresolved {
		t.Fatal("changed configured instance reused exact lookup")
	}
	if _, err := fresh.Lookup(context.Background(), "../"+ref); err != ErrUnresolved {
		t.Fatal("reference traversal accepted")
	}
	if err := os.Chmod(recordPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Lookup(context.Background(), ref); err != ErrUnresolved {
		t.Fatal("unsafe secret file accepted")
	}
	if err := os.Chmod(recordPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Lookup(context.Background(), ref); err != ErrUnresolved {
		t.Fatal("changed record accepted")
	}
	if _, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumGame)); err != ErrProvider {
		t.Fatal("corrupt preexisting record overwritten")
	}
	if err := os.Rename(recordPath, recordPath+"-lost"); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Lookup(context.Background(), ref); err != ErrUnresolved {
		t.Fatal("lost record silently rediscovered")
	}
	if err := os.Symlink(recordPath+"-lost", recordPath); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Lookup(context.Background(), ref); err != ErrUnresolved {
		t.Fatal("symlink reference followed")
	}
}

func TestProwlarrRejectsUnsafeLocatorsAndSanitizesPresentation(t *testing.T) {
	p := newTestProwlarr(t, "http://example.invalid/prefix")
	valid := "http://example.invalid/prefix/3/download?link=Protected_AES&file=Release&apikey=" + prowlarrTestKey
	for _, locator := range []string{
		"https://foreign.invalid/3/download?link=abc", "magnet:?xt=urn:btih:private", "/prefix/3/download?link=abc", "http://user:pass@example.invalid/prefix/3/download?link=abc", "http://example.invalid/3/download?link=abc", "http://example.invalid/prefix/4/download?link=abc", valid + "&unknown=secret", valid + "&apikey=another", valid + "#fragment", "http://example.invalid/prefix/3/download?link=abc&apikey=WRONG", "http://example.invalid/prefix/3/download?link=" + prowlarrTestKey, "http://example.invalid/prefix/3/download?link=abc&file=" + prowlarrTestKey,
	} {
		if _, err := p.normalizeLocator(locator, 3); err != ErrInvalidResponse {
			t.Fatal("unsafe locator accepted")
		}
	}
	for _, route := range []string{"/prefix/3/download", "/prefix/api/v1/indexer/3/download"} {
		l, err := p.normalizeLocator("http://example.invalid"+route+"?link=abc&file=Real", 3)
		if err != nil || l.Route != route || l.Link != "abc" {
			t.Fatal("documented proxy rejected")
		}
	}
	for _, title := range []string{"", "https://tracker.invalid/passkey", prowlarrTestKey, "Title at example.invalid", "Title\ncontrol", "Title\u202eSECRET", "magnet:?xt=private", "www.foreign.invalid"} {
		if got := p.safeTitle(title, strings.Repeat("a", 64)); got != "Prowlarr release aaaaaaaaaaaa" {
			t.Fatal("unsuitable title not replaced")
		}
	}
	for _, title := range []string{"Real.Release.2026.1080p", "日本語のタイトル", "Title: subtitle"} {
		if got := p.safeTitle(title, strings.Repeat("a", 64)); got != title {
			t.Fatal("valid release presentation unnecessarily discarded")
		}
	}
}
