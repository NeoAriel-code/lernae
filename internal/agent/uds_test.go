package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUDSGetStatusAndGracefulSocketCleanup(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "run", "agent.sock")
	server, err := ListenUDS(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve() }()

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket permissions = %04o, want 0600", got)
	}
	status, err := (UDSClient{SocketPath: socketPath, Timeout: time.Second}).GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateOnline || status.StartedAt.IsZero() {
		t.Fatalf("GetStatus response = %#v", status)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveResult; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after shutdown, Lstat error = %v", err)
	}
}

func TestUDSClientHandlesOfflineAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sock")
	_, err := (UDSClient{SocketPath: path, Timeout: 100 * time.Millisecond}).GetStatus(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Agent socket") {
		t.Fatalf("offline Agent error = %v", err)
	}
}

func TestUDSRejectsUnknownAndGenericCommands(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	server, err := ListenUDS(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve() }()
	defer func() {
		_ = server.Close()
		<-serveResult
	}()

	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sideEffectPath := filepath.Join(t.TempDir(), "should-not-exist")
	if err := json.NewEncoder(conn).Encode(map[string]string{
		"operation": "run_command",
		"command":   "touch " + sideEffectPath,
	}); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if response.Error == "" || response.Status != nil {
		t.Fatalf("unknown operation response = %#v", response)
	}
	if _, err := os.Stat(sideEffectPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected command side effect, stat error = %v", err)
	}
}

func TestListenUDSRefusesActiveSocketAndReplacesStaleSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	first, err := ListenUDS(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ListenUDS(socketPath); err == nil {
		t.Fatal("expected a second active listener to be refused")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	address := &net.UnixAddr{Name: socketPath, Net: "unix"}
	stale, err := net.ListenUnix("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := ListenUDS(socketPath)
	if err != nil {
		t.Fatalf("replace stale socket: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestListenUDSRefusesRegularFileAndPathTraversal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenUDS(path); err == nil {
		t.Fatal("expected regular file at socket path to be refused")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep" {
		t.Fatalf("socket setup changed the regular file: data=%q err=%v", data, err)
	}
	if _, err := ListenUDS(t.TempDir() + "/../unsafe.sock"); err == nil {
		t.Fatal("expected path traversal to be rejected")
	}
}

func TestSocketPathCannotUnlinkReplacementOnShutdown(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	server, err := ListenUDS(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	replacement.SetUnlinkOnClose(false)
	defer func() {
		_ = replacement.Close()
		_ = os.Remove(socketPath)
	}()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("shutdown removed a replacement socket: %v", err)
	}
}

func TestUDSShutdownRacesSafelyWithStatusRequests(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	server, err := ListenUDS(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve() }()

	var requests sync.WaitGroup
	for range 20 {
		requests.Add(1)
		go func() {
			defer requests.Done()
			_, _ = (UDSClient{SocketPath: socketPath, Timeout: time.Second}).GetStatus(context.Background())
		}()
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	requests.Wait()
	if err := <-serveResult; err != nil {
		t.Fatal(err)
	}
}

type restoreExecutorFunc func(context.Context, RestoreAsset, func(RestoreProgress) error) (RestoreResult, error)

func (executor restoreExecutorFunc) Restore(ctx context.Context, request RestoreAsset, progress func(RestoreProgress) error) (RestoreResult, error) {
	return executor(ctx, request, progress)
}

func TestUDSRestoreAssetStreamsTypedProgressAndResult(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	request := validRestoreRequest()
	want := RestoreResult{
		AssetID:           request.Asset.ID,
		LocationClass:     "local_cache",
		RelativePath:      "assets/asset-1/game.iso",
		VerifiedSizeBytes: 4,
		VerifiedAt:        time.Now().UTC(),
		LocalReady:        true,
	}
	server, err := ListenUDS(socketPath, restoreExecutorFunc(func(_ context.Context, got RestoreAsset, progress func(RestoreProgress) error) (RestoreResult, error) {
		if got.JobID != request.JobID || got.SourceLocation != request.SourceLocation {
			t.Errorf("RestoreAsset request = %#v, want %#v", got, request)
		}
		if err := progress(RestoreProgress{Phase: RestorePhaseValidating, TotalBytes: 4}); err != nil {
			return RestoreResult{}, err
		}
		if err := progress(RestoreProgress{Phase: RestorePhaseCopying, CurrentBytes: 4, TotalBytes: 4}); err != nil {
			return RestoreResult{}, err
		}
		if err := progress(RestoreProgress{Phase: RestorePhaseVerifying, CurrentBytes: 4, TotalBytes: 4}); err != nil {
			return RestoreResult{}, err
		}
		if err := progress(RestoreProgress{Phase: RestorePhasePromoting, CurrentBytes: 4, TotalBytes: 4}); err != nil {
			return RestoreResult{}, err
		}
		if err := progress(RestoreProgress{Phase: RestorePhaseComplete, CurrentBytes: 4, TotalBytes: 4}); err != nil {
			return RestoreResult{}, err
		}
		return want, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve() }()
	defer func() {
		_ = server.Close()
		<-serveResult
	}()

	var progressEvents []RestoreProgress
	got, err := (UDSClient{SocketPath: socketPath}).RestoreAsset(context.Background(), request, func(progress RestoreProgress) {
		progressEvents = append(progressEvents, progress)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(progressEvents) != 5 || progressEvents[0].Phase != RestorePhaseValidating || progressEvents[1].Phase != RestorePhaseCopying || progressEvents[4].Phase != RestorePhaseComplete {
		t.Fatalf("restore progress events = %#v", progressEvents)
	}
	if err := got.ValidateFor(request); err != nil {
		t.Fatalf("UDS restore result invalid: %v", err)
	}
}

func TestUDSRestoreCancellationReachesExecutor(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	request := validRestoreRequest()
	executorCancelled := make(chan struct{})
	allowCleanupToFinish := make(chan struct{})
	executorFinished := make(chan struct{})
	server, err := ListenUDS(socketPath, restoreExecutorFunc(func(ctx context.Context, _ RestoreAsset, progress func(RestoreProgress) error) (RestoreResult, error) {
		if err := progress(RestoreProgress{Phase: RestorePhaseCopying, CurrentBytes: 1, TotalBytes: 4}); err != nil {
			return RestoreResult{}, err
		}
		<-ctx.Done()
		close(executorCancelled)
		<-allowCleanupToFinish
		close(executorFinished)
		return RestoreResult{}, ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve() }()
	t.Cleanup(func() {
		_ = server.Close()
		<-serveResult
	})
	var releaseOnce sync.Once
	releaseCleanup := func() { releaseOnce.Do(func() { close(allowCleanupToFinish) }) }
	t.Cleanup(releaseCleanup)

	ctx, cancel := context.WithCancel(context.Background())
	gotResult := make(chan error, 1)
	go func() {
		_, err := (UDSClient{SocketPath: socketPath}).RestoreAsset(ctx, request, func(RestoreProgress) { cancel() })
		gotResult <- err
	}()
	select {
	case <-executorCancelled:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not reach restore executor")
	}
	select {
	case err := <-gotResult:
		t.Fatalf("UDS client returned before Agent cancellation cleanup: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	releaseCleanup()
	if err := <-gotResult; err == nil {
		t.Fatal("cancelled UDS restore unexpectedly succeeded")
	}
	select {
	case <-executorFinished:
	case <-time.After(time.Second):
		t.Fatal("restore executor did not finish its cancellation cleanup")
	}
}

func TestUDSRestoreDeadlineWaitsForCleanupAcknowledgement(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	request := validRestoreRequest()
	executorStarted := make(chan struct{})
	executorCancelled := make(chan struct{})
	allowCleanupToFinish := make(chan struct{})
	executorFinished := make(chan struct{})
	server, err := ListenUDS(socketPath, restoreExecutorFunc(func(ctx context.Context, _ RestoreAsset, progress func(RestoreProgress) error) (RestoreResult, error) {
		for _, event := range []RestoreProgress{
			{Phase: RestorePhaseValidating, TotalBytes: 4},
			{Phase: RestorePhaseStaging, TotalBytes: 4},
			{Phase: RestorePhaseCopying, CurrentBytes: 1, TotalBytes: 4},
		} {
			if err := progress(event); err != nil {
				return RestoreResult{}, err
			}
		}
		close(executorStarted)
		<-ctx.Done()
		close(executorCancelled)
		<-allowCleanupToFinish
		close(executorFinished)
		return RestoreResult{}, ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve() }()
	t.Cleanup(func() {
		_ = server.Close()
		<-serveResult
	})
	var releaseOnce sync.Once
	releaseCleanup := func() { releaseOnce.Do(func() { close(allowCleanupToFinish) }) }
	t.Cleanup(releaseCleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	type callResult struct {
		result RestoreResult
		err    error
	}
	gotResult := make(chan callResult, 1)
	go func() {
		result, err := (UDSClient{SocketPath: socketPath}).RestoreAsset(ctx, request, nil)
		gotResult <- callResult{result: result, err: err}
	}()
	select {
	case <-executorStarted:
	case <-time.After(time.Second):
		t.Fatal("restore executor did not enter copy before caller deadline")
	}
	<-ctx.Done()
	select {
	case <-executorCancelled:
	case <-time.After(time.Second):
		t.Fatal("caller deadline did not reach restore executor")
	}
	select {
	case got := <-gotResult:
		t.Fatalf("UDS client returned before delayed cleanup acknowledgement: result=%#v error=%v", got.result, got.err)
	case <-time.After(40 * time.Millisecond):
	}
	releaseCleanup()
	got := <-gotResult
	if !errors.Is(got.err, context.DeadlineExceeded) || got.result.LocalReady {
		t.Fatalf("deadline restore = %#v, %v; want interrupted deadline without readiness", got.result, got.err)
	}
	select {
	case <-executorFinished:
	case <-time.After(time.Second):
		t.Fatal("restore executor did not finish cleanup after acknowledgement")
	}
}
