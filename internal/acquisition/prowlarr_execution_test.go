package acquisition

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func storedProwlarrSelection(t *testing.T, p *Prowlarr, mutate func(*prowlarrRecord)) Selection {
	t.Helper()
	record := prowlarrRecord{Version: 1, Instance: p.instance, IndexerID: 4, GUID: "private-guid", Protocol: ProwlarrProtocolTorrent, Title: "Exact release", Download: prowlarrLocator{Route: p.base.Path + "/api/v1/indexer/4/download", Link: "CapturedProtectedLink", File: "release.torrent"}}
	if mutate != nil {
		mutate(&record)
	}
	store, err := p.openRecords(true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	if err := p.saveRecord(context.Background(), store, record); err != nil {
		t.Fatal(err)
	}
	return Selection{Candidate: Candidate{JobID: "job-1", EditionID: "edition-1", ProviderID: ProwlarrProviderID, Handle: strings.Repeat("b", 64), Option: Option{ID: record.candidate(), ExecutionRef: record.reference(), Metadata: record.metadata()}}}
}

func TestProwlarrResolutionLocalOnlyExactAuthority(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/prefix/api/v1/indexer/4/download" || r.URL.Query().Get("link") != "CapturedProtectedLink" || r.URL.Query().Get("file") != "release.torrent" || len(r.URL.Query()) != 2 || r.Header.Get("X-Api-Key") != "ROTATED_KEY" {
			t.Error("transport substituted locator or reused stored authorization")
		}
		_, _ = w.Write(testTorrent())
	}))
	defer server.Close()
	p := newTestProwlarr(t, server.URL+"/prefix")
	selection := storedProwlarrSelection(t, p, nil)
	p.config.APIKey = "ROTATED_KEY"
	resolver := NewProwlarrExecutionResolver(p)
	plan, err := resolver.Resolve(context.Background(), selection)
	if err != nil || plan != PlanForSelection(selection) || calls.Load() != 0 {
		t.Fatal("exact local resolution failed or did HTTP")
	}
	payload, err := p.fetchTorrent(context.Background(), plan)
	if err != nil || payload.hash == "" || calls.Load() != 1 {
		t.Fatal("exact postclaim transport failed")
	}
	for _, mutate := range []func(*Selection){
		func(s *Selection) { s.Candidate.Option.ID = strings.Repeat("c", 64) },
		func(s *Selection) { s.Candidate.Option.ExecutionRef = strings.Repeat("d", 64) },
		func(s *Selection) { s.Candidate.ProviderID = "local" },
		func(s *Selection) { s.Candidate.Handle = "bad" },
	} {
		changed := selection
		mutate(&changed)
		if _, err := resolver.Resolve(context.Background(), changed); err != ErrUnresolved {
			t.Fatal("changed exact authority accepted")
		}
	}
	other := newTestProwlarr(t, server.URL+"/different")
	other.config.ReferenceDirectory = p.config.ReferenceDirectory
	if _, err := NewProwlarrExecutionResolver(other).Resolve(context.Background(), selection); err != ErrUnresolved {
		t.Fatal("wrong instance accepted")
	}
	path := p.config.ReferenceDirectory + "/" + selection.Candidate.Option.ExecutionRef
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), selection); err != ErrUnresolved || calls.Load() != 1 {
		t.Fatal("tampered record accepted or rediscovered")
	}
}

func TestProwlarrResolutionRejectsProtocolAndIncompleteLocator(t *testing.T) {
	for _, mutate := range []func(*prowlarrRecord){
		func(r *prowlarrRecord) { r.Protocol = ProwlarrProtocolUsenet },
		func(r *prowlarrRecord) { r.Protocol = ProwlarrProtocolUnknown },
		func(r *prowlarrRecord) { r.Download = prowlarrLocator{} },
		func(r *prowlarrRecord) { r.Download.File = "" },
	} {
		p := newTestProwlarr(t, "http://127.0.0.1:1")
		s := storedProwlarrSelection(t, p, mutate)
		if _, err := NewProwlarrExecutionResolver(p).Resolve(context.Background(), s); err != ErrUnresolved {
			t.Fatal("protocol/missing locator accepted")
		}
	}
}

func TestProwlarrLocatorChoiceIsDeterministicWithoutFallback(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("link") != "CapturedProtectedLink" {
			t.Error("fallback locator selected")
		}
		w.WriteHeader(503)
	}))
	defer server.Close()
	p := newTestProwlarr(t, server.URL)
	s := storedProwlarrSelection(t, p, func(r *prowlarrRecord) {
		r.Magnet = prowlarrLocator{Route: "/4/download", Link: "OtherCapturedLink", File: "other.torrent"}
	})
	if _, err := p.fetchTorrent(context.Background(), PlanForSelection(s)); err == nil || calls.Load() != 1 {
		t.Fatal("failed selected locator triggered fallback/retry")
	}
}

func TestProwlarrTransportRejectsRedirectAndBounds(t *testing.T) {
	for _, mode := range []string{"upstream", "oversize", "invalid", "magnet", "unsupported magnet"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch mode {
				case "upstream":
					w.Header().Set("Location", "/should-not-follow")
					w.WriteHeader(301)
				case "oversize":
					_, _ = w.Write([]byte(strings.Repeat("x", torrentMaxBytes+1)))
				case "invalid":
					_, _ = w.Write([]byte("private raw error"))
				case "magnet":
					w.Header().Set("Location", "magnet:?xt=urn:btih:"+strings.Repeat("a", 40))
					w.WriteHeader(301)
				case "unsupported magnet":
					w.Header().Set("Location", "magnet:?xt=urn:btmh:unsupported")
					w.WriteHeader(301)
				}
			}))
			defer server.Close()
			p := newTestProwlarr(t, server.URL)
			s := storedProwlarrSelection(t, p, nil)
			payload, err := p.fetchTorrent(context.Background(), PlanForSelection(s))
			if mode == "magnet" {
				if err != nil || payload.magnet == "" {
					t.Fatal("supported exact magnet redirect rejected")
				}
			} else if err != ErrInvalidResponse {
				t.Fatal("invalid transport accepted or unsafe error")
			}
			if calls.Load() != 1 {
				t.Fatal("redirect/fallback followed")
			}
		})
	}
}
