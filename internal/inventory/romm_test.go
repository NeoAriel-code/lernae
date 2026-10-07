package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lernae/internal/domain"
	"lernae/internal/providers"
)

const (
	rommTestToken      = "rmm_test_secret_do_not_leak"
	rommTestPlatformID = int64(21)
	rommTestIGDBID     = int64(1565)
)

func TestRomMFindMatchingAssetsUsesExactIGDBIdentityAndGameCubeEvidence(t *testing.T) {
	var romQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+rommTestToken {
			t.Errorf("Authorization header = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/platforms":
			writeJSON(t, w, []any{map[string]any{"id": rommTestPlatformID, "slug": "ngc"}})
		case "/api/roms":
			romQuery = r.URL.Query()
			item := validRomMItem(501, rommTestIGDBID)
			item["fs_name"] = "/private/romm/Soulcalibur II.iso"
			writeJSON(t, w, rommPage(1, 100, 0, []any{item}))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	source := newRomMTestSource(t, server, RomMInventorySourceOptions{})
	matches, err := source.FindMatchingAssets(context.Background(), providers.InventoryQuery{
		WorkIdentity: providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: "1565"},
		Platform:     "gamecube",
		Format:       "disc_image",
	})
	if err != nil {
		t.Fatalf("FindMatchingAssets() error = %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("FindMatchingAssets() returned %d matches, want 1", len(matches))
	}
	match := matches[0]
	if match.AssetID != domain.AssetID("romm-501") || match.Platform != "gamecube" || match.Format != "disc_image" || match.TotalSizeBytes != 1450000000 {
		t.Fatalf("match = %#v", match)
	}
	if len(match.Parts) != 1 || match.Parts[0].Role != "rom" || match.Parts[0].Filename != "romm-501.iso" || match.Parts[0].SizeBytes != 1450000000 || match.Parts[0].RelativePath != "" {
		t.Fatalf("parts = %#v", match.Parts)
	}
	if got := match.Parts[0].Filename; strings.ContainsAny(got, "/\\") {
		t.Fatalf("RomM filename became a path: %q", got)
	}
	if romQuery.Get("platform_ids") != strconv.FormatInt(rommTestPlatformID, 10) ||
		romQuery.Get("metadata_providers") != "igdb" ||
		romQuery.Get("group_by_meta_id") != "false" ||
		romQuery.Get("with_files") != "false" ||
		romQuery.Get("with_char_index") != "false" ||
		romQuery.Get("with_filter_values") != "false" ||
		romQuery.Get("limit") != "100" || romQuery.Get("offset") != "0" {
		t.Fatalf("RomM query = %v", romQuery)
	}
	if strings.Contains(fmt.Sprint(match), "/private/romm/") {
		t.Fatalf("RomM path escaped into provider-neutral match: %#v", match)
	}
}

func TestRomMFindMatchingAssetsRequiresExactIdentityAndSupportedEdition(t *testing.T) {
	requests := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/api/platforms" {
			writeJSON(t, w, []any{map[string]any{"id": rommTestPlatformID, "slug": "ngc"}})
			return
		}
		writeJSON(t, w, rommPage(1, 100, 0, []any{
			validRomMItem(501, 999),
		}))
	}))
	defer server.Close()
	source := newRomMTestSource(t, server, RomMInventorySourceOptions{})

	tests := []struct {
		name  string
		query providers.InventoryQuery
	}{
		{name: "same title with another IGDB identity", query: rommQuery("1565")},
		{name: "wrong identity provider", query: providers.InventoryQuery{WorkIdentity: providers.ExternalWorkIdentity{Provider: "IGDB", ExternalID: "1565"}, Platform: "gamecube", Format: "disc_image"}},
		{name: "unsupported platform", query: providers.InventoryQuery{WorkIdentity: providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: "1565"}, Platform: "playstation2", Format: "disc_image"}},
		{name: "unsupported format", query: providers.InventoryQuery{WorkIdentity: providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: "1565"}, Platform: "gamecube", Format: "gcm"}},
		{name: "noncanonical numeric identity", query: providers.InventoryQuery{WorkIdentity: providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: "01565"}, Platform: "gamecube", Format: "disc_image"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			matches, err := source.FindMatchingAssets(context.Background(), test.query)
			if err != nil {
				t.Fatalf("FindMatchingAssets() error = %v", err)
			}
			if len(matches) != 0 {
				t.Fatalf("FindMatchingAssets() = %#v, want no exact match", matches)
			}
		})
	}
	if requests.Load() == 0 {
		t.Fatal("valid-shaped exact lookup never queried RomM")
	}
}

func TestRomMFindMatchingAssetsRejectsUnsupportedRomShapes(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string]any)
	}{
		{name: "missing IGDB identity", edit: func(item map[string]any) { delete(item, "igdb_id") }},
		{name: "non-ISO format", edit: func(item map[string]any) { item["fs_extension"] = "gcm" }},
		{name: "unknown size", edit: func(item map[string]any) { item["fs_size_bytes"] = 0 }},
		{name: "not a simple single file", edit: func(item map[string]any) { item["has_simple_single_file"] = false }},
		{name: "nested single file", edit: func(item map[string]any) { item["has_nested_single_file"] = true }},
		{name: "multiple files", edit: func(item map[string]any) { item["has_multiple_files"] = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := validRomMItem(501, rommTestIGDBID)
			test.edit(item)
			server := newSingleItemRomMServer(t, item)
			defer server.Close()
			source := newRomMTestSource(t, server, RomMInventorySourceOptions{})
			matches, err := source.FindMatchingAssets(context.Background(), rommQuery("1565"))
			if err != nil {
				t.Fatalf("FindMatchingAssets() error = %v", err)
			}
			if len(matches) != 0 {
				t.Fatalf("FindMatchingAssets() = %#v, want unsupported record to fail closed", matches)
			}
		})
	}
}

func TestRomMFindMatchingAssetsRejectsAmbiguousDuplicateIdentity(t *testing.T) {
	server := newSingleItemRomMServer(t, validRomMItem(501, rommTestIGDBID), validRomMItem(502, rommTestIGDBID))
	defer server.Close()
	source := newRomMTestSource(t, server, RomMInventorySourceOptions{})

	_, err := source.FindMatchingAssets(context.Background(), rommQuery("1565"))
	if !errors.Is(err, ErrRomMAmbiguousInventory) {
		t.Fatalf("FindMatchingAssets() error = %v, want ErrRomMAmbiguousInventory", err)
	}
}

func TestRomMFindMatchingAssetsPaginatesAndValidatesStablePageMetadata(t *testing.T) {
	var offsets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/platforms" {
			writeJSON(t, w, []any{map[string]any{"id": rommTestPlatformID, "slug": "ngc"}})
			return
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		offsets = append(offsets, strconv.Itoa(offset))
		items := make([]any, 0, 100)
		start, end := offset, offset+100
		if end > 101 {
			end = 101
		}
		for id := start; id < end; id++ {
			igdbID := int64(9000 + id)
			if id == 100 {
				igdbID = rommTestIGDBID
			}
			items = append(items, validRomMItem(int64(id+1), igdbID))
		}
		writeJSON(t, w, rommPage(101, 100, offset, items))
	}))
	defer server.Close()
	source := newRomMTestSource(t, server, RomMInventorySourceOptions{})

	matches, err := source.FindMatchingAssets(context.Background(), rommQuery("1565"))
	if err != nil {
		t.Fatalf("FindMatchingAssets() error = %v", err)
	}
	if len(matches) != 1 || matches[0].AssetID != "romm-101" {
		t.Fatalf("matches = %#v", matches)
	}
	if strings.Join(offsets, ",") != "0,100" {
		t.Fatalf("page offsets = %v, want [0 100]", offsets)
	}
}

func TestRomMFindMatchingAssetsRejectsUnboundedOrInconsistentPages(t *testing.T) {
	tests := []struct {
		name string
		page func(*http.Request) any
	}{
		{name: "record total exceeds cap", page: func(*http.Request) any { return rommPage(10001, 100, 0, nil) }},
		{name: "page offset disagrees with request", page: func(*http.Request) any { return rommPage(1, 100, 99, []any{validRomMItem(1, 1)}) }},
		{name: "page total inconsistent", page: func(*http.Request) any { return rommPage(1, 99, 0, []any{validRomMItem(1, 1)}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/platforms" {
					writeJSON(t, w, []any{map[string]any{"id": rommTestPlatformID, "slug": "ngc"}})
					return
				}
				writeJSON(t, w, test.page(r))
			}))
			defer server.Close()
			source := newRomMTestSource(t, server, RomMInventorySourceOptions{})
			_, err := source.FindMatchingAssets(context.Background(), rommQuery("1565"))
			if !errors.Is(err, ErrRomMInvalidResponse) && !errors.Is(err, ErrRomMResponseTooLarge) {
				t.Fatalf("FindMatchingAssets() error = %v, want bounded-response error", err)
			}
		})
	}
}

func TestRomMFindMatchingAssetsRejectsChangingPaginationTotals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/platforms" {
			writeJSON(t, w, []any{map[string]any{"id": rommTestPlatformID, "slug": "ngc"}})
			return
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if offset == 0 {
			items := make([]any, 100)
			for i := range items {
				items[i] = validRomMItem(int64(i+1), int64(i+1000))
			}
			writeJSON(t, w, rommPage(101, 100, 0, items))
			return
		}
		writeJSON(t, w, rommPage(102, 100, 100, []any{validRomMItem(101, rommTestIGDBID)}))
	}))
	defer server.Close()
	source := newRomMTestSource(t, server, RomMInventorySourceOptions{})
	_, err := source.FindMatchingAssets(context.Background(), rommQuery("1565"))
	if !errors.Is(err, ErrRomMInvalidResponse) {
		t.Fatalf("FindMatchingAssets() error = %v, want ErrRomMInvalidResponse", err)
	}
}

func TestRomMFindMatchingAssetsTreatsMissingPlatformAsEmpty(t *testing.T) {
	var romRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/platforms" {
			writeJSON(t, w, []any{map[string]any{"id": int64(8), "slug": "ps2"}})
			return
		}
		romRequests.Add(1)
		t.Errorf("unexpected ROM request without ngc platform")
	}))
	defer server.Close()
	source := newRomMTestSource(t, server, RomMInventorySourceOptions{})
	matches, err := source.FindMatchingAssets(context.Background(), rommQuery("1565"))
	if err != nil || len(matches) != 0 || romRequests.Load() != 0 {
		t.Fatalf("matches=%#v err=%v rom_requests=%d", matches, err, romRequests.Load())
	}
}

func TestRomMFindMatchingAssetsTreatsEmptyInventoryAsNoMatch(t *testing.T) {
	server := newSingleItemRomMServer(t)
	defer server.Close()
	source := newRomMTestSource(t, server, RomMInventorySourceOptions{})
	matches, err := source.FindMatchingAssets(context.Background(), rommQuery("1565"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("FindMatchingAssets() = %#v, %v; want empty safe result", matches, err)
	}
}

func TestRomMFindMatchingAssetsRejectsMalformedResponseWithoutEchoingBody(t *testing.T) {
	const responseSecret = "private-response-body"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, "{malformed %s", responseSecret)
	}))
	defer server.Close()
	source := newRomMTestSource(t, server, RomMInventorySourceOptions{})
	_, err := source.FindMatchingAssets(context.Background(), rommQuery("1565"))
	if !errors.Is(err, ErrRomMInvalidResponse) {
		t.Fatalf("FindMatchingAssets() error = %v, want ErrRomMInvalidResponse", err)
	}
	if strings.Contains(err.Error(), responseSecret) || strings.Contains(err.Error(), rommTestToken) {
		t.Fatalf("error leaked response data: %v", err)
	}
}

func TestRomMFindMatchingAssetsRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/platforms" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(strings.Repeat(" ", 8<<20+1)))
			return
		}
		t.Errorf("unexpected request after oversized response: %s", r.URL.Path)
	}))
	defer server.Close()
	source := newRomMTestSource(t, server, RomMInventorySourceOptions{})

	_, err := source.FindMatchingAssets(context.Background(), rommQuery("1565"))
	if !errors.Is(err, ErrRomMResponseTooLarge) {
		t.Fatalf("FindMatchingAssets() error = %v, want ErrRomMResponseTooLarge", err)
	}
}

func TestRomMFindMatchingAssetsMapsProviderFailuresWithoutLeakingSecrets(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, "credential leak "+rommTestToken+" response secret")
			}))
			defer server.Close()
			source := newRomMTestSource(t, server, RomMInventorySourceOptions{})
			_, err := source.FindMatchingAssets(context.Background(), rommQuery("1565"))
			if err == nil {
				t.Fatal("FindMatchingAssets() unexpectedly succeeded")
			}
			if strings.Contains(err.Error(), rommTestToken) || strings.Contains(err.Error(), "response secret") {
				t.Fatalf("error leaked provider response data: %v", err)
			}
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				if !errors.Is(err, ErrRomMAuthentication) {
					t.Fatalf("error = %v, want ErrRomMAuthentication", err)
				}
			} else if !errors.Is(err, ErrRomMProviderUnavailable) {
				t.Fatalf("error = %v, want ErrRomMProviderUnavailable", err)
			}
		})
	}
}

func TestNewRomMInventorySourceValidatesConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		base  string
		token string
	}{
		{name: "relative URL", base: "/romm", token: "valid-token"},
		{name: "unsupported URL scheme", base: "file:///romm", token: "valid-token"},
		{name: "URL credentials", base: "https://user:password@example.test", token: "valid-token"},
		{name: "query in base URL", base: "https://example.test/?token=secret", token: "valid-token"},
		{name: "missing token", base: "https://example.test", token: ""},
		{name: "header injection token", base: "https://example.test", token: "valid\r\ntoken"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRomMInventorySource(RomMInventorySourceOptions{BaseURL: test.base, Token: test.token})
			if !errors.Is(err, ErrRomMInvalidConfiguration) {
				t.Fatalf("NewRomMInventorySource() error = %v, want ErrRomMInvalidConfiguration", err)
			}
		})
	}
}

func TestRomMSourceDiagnosticsNeverRevealCredentials(t *testing.T) {
	options := RomMInventorySourceOptions{
		BaseURL: "https://romm.example.invalid/synthetic-base-marker",
		Token:   "synthetic-romm-diagnostic-token",
	}
	source, err := NewRomMInventorySource(options)
	if err != nil {
		t.Fatalf("NewRomMInventorySource() error: %v", err)
	}
	for _, formatted := range []string{
		fmt.Sprintf("%v", options), fmt.Sprintf("%+v", options), fmt.Sprintf("%#v", options),
		fmt.Sprintf("%v", source), fmt.Sprintf("%+v", source), fmt.Sprintf("%#v", source),
	} {
		if strings.Contains(formatted, options.BaseURL) || strings.Contains(formatted, options.Token) {
			t.Fatal("RomM source diagnostics exposed a configured value")
		}
	}
	for _, value := range []any{options, source} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), options.BaseURL) || strings.Contains(string(encoded), options.Token) {
			t.Fatal("RomM JSON diagnostics exposed a configured value")
		}
	}

	_, err = NewRomMInventorySource(RomMInventorySourceOptions{BaseURL: options.BaseURL})
	if err == nil {
		t.Fatal("NewRomMInventorySource() accepted a missing token")
	}
	for _, formatted := range []string{fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(formatted, options.BaseURL) || strings.Contains(formatted, options.Token) {
			t.Fatal("RomM configuration error exposed a configured value")
		}
	}
}

func TestRomMInventorySourceHonorsCancellationAndConfiguredTimeout(t *testing.T) {
	t.Run("caller cancellation", func(t *testing.T) {
		started := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
		}))
		defer server.Close()
		source := newRomMTestSource(t, server, RomMInventorySourceOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := source.FindMatchingAssets(ctx, rommQuery("1565"))
			result <- err
		}()
		<-started
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("FindMatchingAssets() error = %v, want context.Canceled", err)
		}
	})

	t.Run("configured request timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer server.Close()
		source := newRomMTestSource(t, server, RomMInventorySourceOptions{RequestTimeout: 20 * time.Millisecond})
		_, err := source.FindMatchingAssets(context.Background(), rommQuery("1565"))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("FindMatchingAssets() error = %v, want context.DeadlineExceeded", err)
		}
	})
}

func TestRomMAssetIdentityIsStableAndEditionEnumerationIsBounded(t *testing.T) {
	server := newSingleItemRomMServer(t, validRomMItem(501, rommTestIGDBID))
	defer server.Close()
	source := newRomMTestSource(t, server, RomMInventorySourceOptions{})
	query := rommQuery("1565")
	first, err := source.FindMatchingAssets(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.FindMatchingAssets(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].AssetID != second[0].AssetID {
		t.Fatalf("asset identity changed: first=%#v second=%#v", first, second)
	}

	assets, err := source.AssetsForEdition(context.Background(), EditionKey("gamecube", "disc_image"))
	if err != nil {
		t.Fatalf("AssetsForEdition() error = %v", err)
	}
	if len(assets) != 1 || assets[0].ID != first[0].AssetID || assets[0].TotalSizeBytes != first[0].TotalSizeBytes {
		t.Fatalf("AssetsForEdition() = %#v", assets)
	}
}

func rommQuery(externalID string) providers.InventoryQuery {
	return providers.InventoryQuery{
		WorkIdentity: providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: externalID},
		Platform:     "gamecube",
		Format:       "disc_image",
	}
}

func validRomMItem(recordID, igdbID int64) map[string]any {
	return map[string]any{
		"id": recordID, "igdb_id": igdbID, "platform_id": rommTestPlatformID, "platform_slug": "ngc",
		"fs_name": "Soulcalibur II", "fs_extension": ".iso", "fs_size_bytes": int64(1450000000),
		"has_simple_single_file": true, "has_nested_single_file": false, "has_multiple_files": false,
		"fs_path": "/private/romm/Soulcalibur II.iso", "full_path": "/private/romm/Soulcalibur II.iso",
		"files": []any{},
	}
}

func rommPage(total, limit, offset int, items []any) map[string]any {
	return map[string]any{"items": items, "total": total, "limit": limit, "offset": offset, "char_index": map[string]any{}, "rom_id_index": []int{}, "filter_values": map[string]any{}}
}

func newSingleItemRomMServer(t *testing.T, items ...map[string]any) *httptest.Server {
	t.Helper()
	encoded := make([]any, 0, len(items))
	for _, item := range items {
		encoded = append(encoded, item)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/platforms" {
			writeJSON(t, w, []any{map[string]any{"id": rommTestPlatformID, "slug": "ngc"}})
			return
		}
		writeJSON(t, w, rommPage(len(encoded), 100, 0, encoded))
	}))
}

func newRomMTestSource(t *testing.T, server *httptest.Server, options RomMInventorySourceOptions) *RomMInventorySource {
	t.Helper()
	options.BaseURL = server.URL
	options.Token = rommTestToken
	source, err := NewRomMInventorySource(options)
	if err != nil {
		t.Fatalf("NewRomMInventorySource() error = %v", err)
	}
	return source
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode fake RomM response: %v", err)
	}
}
