package tvmaze

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSeasonSummaryUsesOneExactDetailsOnlyRequestAndCountsKnownSeasons(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Method+" "+request.URL.Path)
		if request.Method != http.MethodGet || request.URL.Path != "/shows/42/seasons" || request.URL.RawQuery != "" {
			t.Errorf("season request = %s %s?%s, want one GET /shows/42/seasons", request.Method, request.URL.Path, request.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `[{"id":11,"number":1,"episodeOrder":8},{"id":12,"number":2,"episodeOrder":null}]`)
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})

	summary, err := source.GetSeasonSummary(context.Background(), "42")
	if err != nil {
		t.Fatalf("GetSeasonSummary() error = %v", err)
	}
	if summary.SeasonCount != 2 {
		t.Fatalf("season count = %d, want two known seasons", summary.SeasonCount)
	}
	if strings.Join(requests, ",") != "GET /shows/42/seasons" {
		t.Fatalf("request log = %v, want a single Details-only season endpoint call", requests)
	}
}

func TestSeasonSummaryDistinguishesUnknownFromKnownSeasons(t *testing.T) {
	for _, test := range []struct {
		name      string
		body      string
		wantCount int
		wantErr   error
	}{
		{name: "null leaves count unknown", body: `null`, wantErr: ErrDetailsMalformedResponse},
		{name: "empty array reports zero", body: `[]`, wantCount: 0},
		{name: "populated array counts records", body: `[{"id":11},{"id":12}]`, wantCount: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := jsonServer(test.body)
			defer server.Close()
			source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})

			summary, err := source.GetSeasonSummary(context.Background(), "42")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("GetSeasonSummary() error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr == nil && summary.SeasonCount != test.wantCount {
				t.Fatalf("season count = %d, want %d", summary.SeasonCount, test.wantCount)
			}
		})
	}
}

func TestSeasonSummaryRejectsInvalidShowIDWithoutRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		t.Error("invalid show identity must not contact TVMaze")
		http.NotFound(w, request)
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	if _, err := source.GetSeasonSummary(context.Background(), "42/episodes"); err == nil {
		t.Fatal("GetSeasonSummary() accepted an unsafe show ID")
	}
}

func TestSeasonSummaryBoundsProviderResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", MaxResponseBodyBytes+1))
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	if _, err := source.GetSeasonSummary(context.Background(), "42"); err == nil {
		t.Fatal("GetSeasonSummary() accepted an oversized response")
	}
}
