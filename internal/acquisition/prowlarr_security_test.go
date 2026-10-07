package acquisition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"lernae/internal/domain"
)

func TestProwlarrExactIdentityRejectsLossyUnicodeAndCountBounds(t *testing.T) {
	for _, payload := range []string{
		`[{"indexerId":1,"guid":"\ud800"}]`,
		`[{"indexerId":1,"guid":"\udc00"}]`,
		"[" + strings.Repeat(`{"indexerId":1,"guid":"a"},`, prowlarrMaxReleases) + `{"indexerId":1,"guid":"b"}]`,
	} {
		if _, err := decodeProwlarrReleases([]byte(payload)); err != ErrInvalidResponse {
			t.Fatal("lossy or excessive release identity accepted")
		}
	}
	for _, payload := range []string{
		`[{"indexerId":1,"guid":"\ud83d\ude00"}]`,
		`[{"indexerId":1,"guid":"literal\\ud800"}]`,
	} {
		if _, err := decodeProwlarrReleases([]byte(payload)); err != nil {
			t.Fatal("valid Unicode or literal escape rejected")
		}
	}
}

func TestProwlarrTransportPolicyTLSVerificationAndClientDeadline(t *testing.T) {
	p := newTestProwlarr(t, "http://127.0.0.1:1")
	transport := p.client.Transport.(*http.Transport)
	if !transport.DisableKeepAlives || transport.ForceAttemptHTTP2 {
		t.Fatal("transport may transparently retry an idempotent search on a reused connection")
	}
	if p.client.Timeout != 30*time.Second || transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("finite timeout or ordinary TLS verification weakened")
	}
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("untrusted TLS peer received an authenticated request")
	}))
	defer tls.Close()
	p = newTestProwlarr(t, tls.URL)
	if _, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumGame)); err != ErrProvider {
		t.Fatal("TLS error was exposed or certificate trust bypassed")
	}
	joined := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(joined)
	}))
	defer server.Close()
	p = newTestProwlarr(t, server.URL)
	p.client.Timeout = 20 * time.Millisecond // Exercise the HTTP deadline without a 30s sleep.
	if _, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumGame)); err != ErrTimeout {
		t.Fatal("HTTP client timeout lost safe category")
	}
	<-joined
}

func TestProwlarrBodyReadTimeoutPreservesSafeCategory(t *testing.T) {
	joined := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(joined)
	}))
	defer server.Close()
	p := newTestProwlarr(t, server.URL)
	p.client.Timeout = 20 * time.Millisecond
	if _, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumGame)); err != ErrTimeout {
		t.Fatalf("body timeout category = %v, want %s", err, ErrTimeout)
	}
	<-joined
}

func TestProwlarrConcurrentImmutablePublicationAndPrivateRecordTampering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"indexerId":1,"guid":"private-guid","title":"Real Release"}]`))
	}))
	defer server.Close()
	p := newTestProwlarr(t, server.URL)
	var wait sync.WaitGroup
	errors := make(chan error, 12)
	options := make(chan []Option, 12)
	for i := 0; i < 12; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			o, err := p.Discover(context.Background(), prowlarrTarget(domain.MediumGame))
			errors <- err
			options <- o
		}()
	}
	wait.Wait()
	close(errors)
	close(options)
	for err := range errors {
		if err != nil {
			t.Fatal("concurrent publication failed", err)
		}
	}
	var expected []Option
	for o := range options {
		if expected == nil {
			expected = o
		}
		if !reflect.DeepEqual(o, expected) {
			t.Fatal("concurrent exact snapshot changed")
		}
	}
	entries, err := os.ReadDir(p.config.ReferenceDirectory)
	if err != nil || len(entries) != 1 {
		t.Fatal("owned temporary record residue after successful publication")
	}
	store, err := p.openRecords(false)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.readRecord(store, expected[0].ExecutionRef)
	store.close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Lookup(context.Background(), expected[0].ExecutionRef); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*prowlarrRecord){
		func(r *prowlarrRecord) { r.Version = 2 },
		func(r *prowlarrRecord) { r.Instance = strings.Repeat("a", 64) },
		func(r *prowlarrRecord) { r.IndexerID = 0 },
		func(r *prowlarrRecord) { r.GUID = "" },
		func(r *prowlarrRecord) { r.Protocol = "other" },
		func(r *prowlarrRecord) { r.Protocol = "" },
		func(r *prowlarrRecord) { r.Title = p.config.APIKey },
		func(r *prowlarrRecord) { r.Download.Route = "../escape"; r.Download.Link = "abc" },
		func(r *prowlarrRecord) { r.Download.Link = p.config.APIKey },
	} {
		changed := r
		mutate(&changed)
		if err := os.WriteFile(filepath.Join(p.config.ReferenceDirectory, changed.reference()), changed.bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Lookup(context.Background(), changed.reference()); err != ErrUnresolved {
			t.Fatal("content-address-valid but invalid private record accepted")
		}
	}
	// Even content-address-valid legacy numeric or wrong-type protocols must
	// fail closed. No numeric migration can invent documented wire authority.
	for _, protocol := range []string{"0", "1", "2", "true", "null"} {
		data := bytes.Replace(r.bytes(), []byte(`"Protocol":"unknown"`), []byte(`"Protocol":`+protocol), 1)
		ref := prowlarrHash("reference-v1", data)
		if err := os.WriteFile(filepath.Join(p.config.ReferenceDirectory, ref), data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Lookup(context.Background(), ref); err != ErrUnresolved {
			t.Fatal("wrong-type private protocol accepted")
		}
	}
	oversized := []byte(strings.Repeat("x", prowlarrMaxRecordBytes+1))
	ref := prowlarrHash("reference-v1", oversized)
	if err := os.WriteFile(filepath.Join(p.config.ReferenceDirectory, ref), oversized, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Lookup(context.Background(), ref); err != ErrUnresolved {
		t.Fatal("oversized private record accepted")
	}
	// Path replacement and aliases never recover an equivalent private record.
	original := p.config.ReferenceDirectory
	if err := os.Rename(original, original+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original+"-moved", original); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Lookup(context.Background(), expected[0].ExecutionRef); err != ErrUnresolved {
		t.Fatal("symlink provider directory followed")
	}
}

func TestProwlarrPublicationBetweenReadMissAndLstat(t *testing.T) {
	p := newTestProwlarr(t, "http://example.invalid")
	peer, err := NewProwlarr(p.config)
	if err != nil {
		t.Fatal(err)
	}
	store, err := p.openRecords(true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	peerStore, err := peer.openRecords(false)
	if err != nil {
		t.Fatal(err)
	}
	defer peerStore.close()
	r := prowlarrRecord{Version: 1, Instance: p.instance, IndexerID: 1, GUID: "private-guid", Protocol: ProwlarrProtocolUnknown, Title: "Release"}
	fileSyncs, directorySyncs := 0, 0
	p.syncFile = func(file *os.File) error { fileSyncs++; return file.Sync() }
	p.syncDirectory = func(file *os.File) error { directorySyncs++; return file.Sync() }
	miss := make(chan struct{})
	published := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- p.saveRecordWithReadMissHook(context.Background(), store, r, func() {
			close(miss)
			<-published
		})
	}()
	<-miss
	peerErr := peer.saveRecord(context.Background(), peerStore, r)
	before, statErr := os.Stat(filepath.Join(p.config.ReferenceDirectory, r.reference()))
	close(published)
	writerErr := <-result
	if peerErr != nil || statErr != nil {
		t.Fatal("peer publication failed", peerErr, statErr)
	}
	if writerErr != nil {
		t.Fatal("verified peer publication in read-miss window rejected", writerErr)
	}
	if fileSyncs != 1 || directorySyncs != 1 {
		t.Fatal("peer reuse did not sync both the exact file and directory")
	}
	after, err := os.Stat(filepath.Join(p.config.ReferenceDirectory, r.reference()))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("peer's immutable entry was replaced")
	}
	confirmed, err := p.readRecord(store, r.reference())
	if err != nil || confirmed != r {
		t.Fatal("peer's exact record was not retained")
	}
	entries, err := os.ReadDir(p.config.ReferenceDirectory)
	if err != nil || len(entries) != 1 {
		t.Fatal("read-miss arbitration left partial residue")
	}
}

func TestProwlarrReadMissPeerArbitrationFailsClosed(t *testing.T) {
	for _, scenario := range []string{"malformed", "noncanonical", "different instance", "unsafe mode", "hardlink", "symlink", "directory", "file sync failure", "directory sync failure", "replacement during sync"} {
		t.Run(scenario, func(t *testing.T) {
			p := newTestProwlarr(t, "http://example.invalid")
			peer, err := NewProwlarr(p.config)
			if err != nil {
				t.Fatal(err)
			}
			store, err := p.openRecords(true)
			if err != nil {
				t.Fatal(err)
			}
			defer store.close()
			peerStore, err := peer.openRecords(false)
			if err != nil {
				t.Fatal(err)
			}
			defer peerStore.close()
			r := prowlarrRecord{Version: 1, Instance: p.instance, IndexerID: 1, GUID: "private-guid", Protocol: ProwlarrProtocolUnknown, Title: "Release"}
			path := filepath.Join(p.config.ReferenceDirectory, r.reference())
			switch scenario {
			case "file sync failure":
				p.syncFile = func(*os.File) error { return errors.New("sync failure") }
			case "directory sync failure":
				p.syncDirectory = func(*os.File) error { return errors.New("sync failure") }
			case "replacement during sync":
				p.syncFile = func(file *os.File) error {
					if err := file.Sync(); err != nil {
						return err
					}
					if err := os.Rename(path, path+"-retained"); err != nil {
						return err
					}
					return os.WriteFile(path, r.bytes(), 0600)
				}
			}
			miss := make(chan struct{})
			published := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				result <- p.saveRecordWithReadMissHook(context.Background(), store, r, func() {
					close(miss)
					<-published
				})
			}()
			<-miss
			// Always release/join the writer, including setup failures.
			peerErr := peer.saveRecord(context.Background(), peerStore, r)
			var mutateErr error
			if peerErr == nil {
				switch scenario {
				case "malformed":
					mutateErr = os.WriteFile(path, []byte("unknown entry"), 0600)
				case "noncanonical":
					mutateErr = os.WriteFile(path, append(r.bytes(), '\n'), 0600)
				case "different instance":
					changed := r
					changed.Instance = strings.Repeat("a", 64)
					mutateErr = os.WriteFile(path, changed.bytes(), 0600)
				case "unsafe mode":
					mutateErr = os.Chmod(path, 0644)
				case "hardlink":
					mutateErr = os.Link(path, path+"-alias")
				case "symlink", "directory":
					mutateErr = os.Rename(path, path+"-retained")
					if mutateErr == nil {
						if scenario == "symlink" {
							mutateErr = os.Symlink(path+"-retained", path)
						} else {
							mutateErr = os.Mkdir(path, 0700)
						}
					}
				}
			}
			before, statErr := os.Lstat(path)
			close(published)
			writerErr := <-result
			if peerErr != nil || mutateErr != nil || statErr != nil {
				t.Fatal("peer setup failed", peerErr, mutateErr, statErr)
			}
			if writerErr != ErrProvider {
				t.Fatal("unsafe or non-durable peer publication accepted", writerErr)
			}
			after, err := os.Lstat(path)
			if err != nil || scenario != "replacement during sync" && (!os.SameFile(before, after) || localFingerprintOf(before) != localFingerprintOf(after)) {
				t.Fatal("arbitration altered the preexisting peer entry")
			}
			entries, err := os.ReadDir(p.config.ReferenceDirectory)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".partial-") {
					t.Fatal("failed arbitration left an owned partial")
				}
			}
		})
	}
}

type prowlarrDeadlineProvider struct {
	*Prowlarr
	budget time.Duration
}

func (p *prowlarrDeadlineProvider) Discover(ctx context.Context, target Target) ([]Option, error) {
	deadline, _ := ctx.Deadline()
	p.budget = time.Until(deadline)
	return p.Prowlarr.Discover(ctx, target)
}

func TestProwlarrCoreUsesBoundedRemoteBudgetAndPublishesSafeInvalidResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"indexerId":1,"guid":"a"},{"indexerId":1,"guid":"a","size":2}]`))
	}))
	defer server.Close()
	p := &prowlarrDeadlineProvider{Prowlarr: newTestProwlarr(t, server.URL)}
	service, _, _, _, job := fixture(t, p)
	result, err := service.Discover(context.Background(), job.ID)
	if err != nil || p.budget <= 2*time.Second || p.budget > 30*time.Second || len(result.Failures) != 1 || result.Failures[0].Category != ErrInvalidResponse || len(result.Candidates) != 0 {
		t.Fatal("core lost the productive adapter's safe response category or discovery budget")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := service.Discover(ctx, job.ID); err != nil || p.budget <= 0 || p.budget > time.Second {
		t.Fatal("provider policy overrode shorter caller deadline")
	}
}

type prowlarrStatInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (i prowlarrStatInfo) Sys() any { return &i.stat }

func TestProwlarrPrivateOwnershipHardlinksAndDurabilityFailureCleanup(t *testing.T) {
	p := newTestProwlarr(t, "http://example.invalid")
	store, err := p.openRecords(true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	r := prowlarrRecord{Version: 1, Instance: p.instance, IndexerID: 1, GUID: "private-guid", Protocol: ProwlarrProtocolUnknown, Title: "Release"}
	unknown := filepath.Join(p.config.ReferenceDirectory, "unknown-entry")
	if err := os.WriteFile(unknown, []byte("preserve preexisting data"), 0600); err != nil {
		t.Fatal(err)
	}
	originalSyncFile, originalSyncDirectory := p.syncFile, p.syncDirectory
	p.syncFile = func(*os.File) error { return errors.New("PRIVATE_FILE_SYNC_FAILURE") }
	if err := p.saveRecord(context.Background(), store, r); err != ErrProvider {
		t.Fatal("failed file durability reported success")
	}
	entries, _ := os.ReadDir(p.config.ReferenceDirectory)
	if len(entries) != 1 || entries[0].Name() != "unknown-entry" {
		t.Fatal("failed unpublished record left temp or removed unknown entry")
	}
	p.syncFile = originalSyncFile
	p.syncDirectory = func(*os.File) error { return errors.New("PRIVATE_DIRECTORY_SYNC_FAILURE") }
	if err := p.saveRecord(context.Background(), store, r); err != ErrProvider {
		t.Fatal("failed directory durability reported success")
	}
	// Published-but-unconfirmed bytes remain. Exact reuse MUST establish the
	// missing durability rather than trusting that the entry already exists.
	if _, err := p.Lookup(context.Background(), r.reference()); err != nil {
		t.Fatal("published record was destructively rolled back")
	}
	if err := p.saveRecord(context.Background(), store, r); err != ErrProvider {
		t.Fatal("existing reference reuse bypassed directory sync")
	}
	p.syncDirectory = originalSyncDirectory
	if err := p.saveRecord(context.Background(), store, r); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(p.config.ReferenceDirectory, r.reference())
	info, err := os.Stat(entry)
	if err != nil {
		t.Fatal(err)
	}
	if !prowlarrPrivateFile(info) {
		t.Fatal("safe actual private file rejected")
	}
	stat := *info.Sys().(*syscall.Stat_t)
	stat.Uid++
	if prowlarrPrivateFile(prowlarrStatInfo{FileInfo: info, stat: stat}) {
		t.Fatal("unowned private file accepted")
	}
	if err := os.Link(entry, entry+"-alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Lookup(context.Background(), r.reference()); err != ErrUnresolved {
		t.Fatal("multiply linked private state accepted")
	}
	data, _ := os.ReadFile(unknown)
	if string(data) != "preserve preexisting data" {
		t.Fatal("unknown entry altered by cleanup")
	}
}

func TestProwlarrCurrentKeyCannotPersistInAnySnapshotString(t *testing.T) {
	p := newTestProwlarr(t, "http://example.invalid/base")
	for _, field := range []string{"guid", "downloadUrl", "magnetUrl"} {
		release := map[string]any{"indexerId": 1, "guid": "safe-guid"}
		release[field] = p.config.APIKey
		data, _ := json.Marshal([]map[string]any{release})
		parsed, err := decodeProwlarrReleases(data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.recordForRelease(parsed[0]); err != ErrInvalidResponse {
			t.Fatal("active key persisted through snapshot string")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Lookup(ctx, strings.Repeat("a", 64)); err != ErrCancelled {
		t.Fatal("private lookup ignored caller cancellation")
	}
	if _, err := p.Discover(ctx, prowlarrTarget(domain.MediumGame)); err != ErrCancelled {
		t.Fatal("discovery ignored caller cancellation")
	}
}
