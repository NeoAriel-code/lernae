package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"lernae/internal/domain"
)

func validLaunchAsset() LaunchAsset {
	return LaunchAsset{
		SessionID: "session-1", WorkID: "work-1", EditionID: "edition-1", AssetID: "asset-1", PartID: "part-1",
		Medium: domain.MediumGame, Platform: "gamecube", Format: "disc_image", Role: "rom",
		Filename: "game.iso", ExpectedBytes: 4,
	}
}

func TestLaunchAssetValidatesSupportedShapeAndContainsOnlyOpaqueIdentity(t *testing.T) {
	request := validLaunchAsset()
	if err := request.Validate(); err != nil {
		t.Fatalf("valid launch request rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*LaunchAsset)
	}{
		{name: "missing session correlation", mutate: func(request *LaunchAsset) { request.SessionID = "" }},
		{name: "unsafe work identity", mutate: func(request *LaunchAsset) { request.WorkID = "../work" }},
		{name: "unsupported medium", mutate: func(request *LaunchAsset) { request.Medium = domain.MediumVideo }},
		{name: "unsupported platform", mutate: func(request *LaunchAsset) { request.Platform = "playstation2" }},
		{name: "unsupported format", mutate: func(request *LaunchAsset) { request.Format = "archive" }},
		{name: "unsupported part role", mutate: func(request *LaunchAsset) { request.Role = "cover" }},
		{name: "unsafe filename", mutate: func(request *LaunchAsset) { request.Filename = "../game.iso" }},
		{name: "unknown expected size", mutate: func(request *LaunchAsset) { request.ExpectedBytes = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := validLaunchAsset()
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid launch request unexpectedly passed validation")
			}
		})
	}

	wire, err := json.Marshal(Request{Operation: OperationLaunchAsset, LaunchAsset: &request})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"path"`, `"local_ready"`, `"ready"`, `"shell"`, `"executable"`, `"flags"`, `"cwd"`, `"env"`, `"signal"`} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("launch wire request contains forbidden field %s: %s", forbidden, wire)
		}
	}
	var decoded Request
	decoder := json.NewDecoder(strings.NewReader(string(wire)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decode typed launch request: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("round-tripped launch request rejected: %v", err)
	}
}

func TestUDSLaunchRejectsCallerSuppliedPathFields(t *testing.T) {
	var starts atomic.Int32
	executor := launchExecutorFunc(func(context.Context, LaunchAsset) (LaunchProcess, error) {
		starts.Add(1)
		return nil, errors.New("launch executor must not see a path-bearing request")
	})
	client, closeServer := startLaunchServer(t, executor)
	defer closeServer()
	conn, err := net.DialTimeout("unix", client.SocketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	wire := `{"operation":"launch_asset","launch_asset":{"session_id":"session-1","work_id":"work-1","edition_id":"edition-1","asset_id":"asset-1","part_id":"part-1","medium":"game","platform":"gamecube","format":"disc_image","role":"rom","filename":"game.iso","expected_bytes":4,"path":"/outside/game.iso"}}` + "\n"
	if _, err := io.WriteString(conn, wire); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "invalid typed request" || response.LaunchStarted != nil || response.LaunchFailed != nil || starts.Load() != 0 {
		t.Fatalf("path-bearing request result = %#v, executor starts=%d", response, starts.Load())
	}
}

func TestUDSLaunchFailureAndTerminalExitEventsAreTypedAndExactlyOnce(t *testing.T) {
	tests := []struct {
		name       string
		exit       LaunchExit
		want       domain.SessionOutcome
		wantCode   *int
		startError bool
	}{
		{name: "start failure only", startError: true},
		{name: "normal exit", exit: launchExit(domain.SessionOutcomeNormalExit, 0), want: domain.SessionOutcomeNormalExit, wantCode: intPointer(0)},
		{name: "nonzero exit", exit: launchExit(domain.SessionOutcomeNonZeroExit, 7), want: domain.SessionOutcomeNonZeroExit, wantCode: intPointer(7)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var waitCalls atomic.Int32
			var starts atomic.Int32
			executor := launchExecutorFunc(func(_ context.Context, got LaunchAsset) (LaunchProcess, error) {
				starts.Add(1)
				if got != validLaunchAsset() {
					t.Errorf("LaunchAsset crossing UDS = %#v, want %#v", got, validLaunchAsset())
				}
				if test.startError {
					return nil, errors.New("sensitive process details must not cross the protocol")
				}
				return launchProcessFunc(func() LaunchExit {
					waitCalls.Add(1)
					return test.exit
				}), nil
			})
			client, closeServer := startLaunchServer(t, executor)
			defer closeServer()

			startEvents := 0
			ended, err := client.LaunchAsset(context.Background(), validLaunchAsset(), func(started LaunchStarted) bool {
				startEvents++
				if test.startError {
					t.Fatal("start failure emitted a started event")
				}
				if started.SessionID != validLaunchAsset().SessionID || started.StartedAt.IsZero() {
					t.Fatalf("started event = %#v", started)
				}
				return true
			})
			if test.startError {
				if !errors.Is(err, ErrLaunchStartFailed) || ended.SessionID != "" {
					t.Fatalf("start failure result = %#v, %v", ended, err)
				}
				if starts.Load() != 1 || waitCalls.Load() != 0 || startEvents != 0 {
					t.Fatalf("failed start side effects: starts=%d waits=%d started-events=%d", starts.Load(), waitCalls.Load(), startEvents)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if startEvents != 1 || starts.Load() != 1 || waitCalls.Load() != 1 {
				t.Fatalf("launch counts: starts=%d waits=%d started-events=%d", starts.Load(), waitCalls.Load(), startEvents)
			}
			if ended.Outcome != test.want || !sameIntPointer(ended.ExitCode, test.wantCode) || ended.EndedAt.IsZero() {
				t.Fatalf("terminal event = %#v, want outcome %q and exit code %v", ended, test.want, test.wantCode)
			}
		})
	}
}

func TestUDSLaunchPreservesTypedLocalCacheInvalidPreStartFailure(t *testing.T) {
	var startCalls atomic.Int32
	executor := launchExecutorFunc(func(context.Context, LaunchAsset) (LaunchProcess, error) {
		startCalls.Add(1)
		return nil, ErrLaunchLocalCacheInvalid
	})
	client, closeServer := startLaunchServer(t, executor)
	defer closeServer()

	var startedEvents atomic.Int32
	ended, err := client.LaunchAsset(context.Background(), validLaunchAsset(), func(LaunchStarted) bool {
		startedEvents.Add(1)
		return true
	})
	if !errors.Is(err, ErrLaunchLocalCacheInvalid) {
		t.Fatalf("local-cache launch error = %v, want typed ErrLaunchLocalCacheInvalid", err)
	}
	if ended.SessionID != "" || startCalls.Load() != 1 || startedEvents.Load() != 0 {
		t.Fatalf("pre-start local-cache failure result=%#v executor-starts=%d started-events=%d", ended, startCalls.Load(), startedEvents.Load())
	}
}

func TestUDSLaunchRejectsSecondActiveLaunchWithTypedConflict(t *testing.T) {
	firstWait := make(chan struct{})
	firstStarted := make(chan struct{})
	var starts atomic.Int32
	executor := launchExecutorFunc(func(_ context.Context, _ LaunchAsset) (LaunchProcess, error) {
		if starts.Add(1) == 1 {
			close(firstStarted)
			return launchProcessFunc(func() LaunchExit {
				<-firstWait
				return launchExit(domain.SessionOutcomeNormalExit, 0)
			}), nil
		}
		return launchProcessFunc(func() LaunchExit { return launchExit(domain.SessionOutcomeNormalExit, 0) }), nil
	})
	client, closeServer := startLaunchServer(t, executor)
	defer closeServer()

	firstResult := make(chan error, 1)
	go func() {
		_, err := client.LaunchAsset(context.Background(), validLaunchAsset(), func(LaunchStarted) bool { return true })
		firstResult <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first launch did not start")
	}
	_, err := client.LaunchAsset(context.Background(), validLaunchAsset(), func(LaunchStarted) bool {
		t.Fatal("rejected second launch must not emit started")
		return false
	})
	if !errors.Is(err, ErrLaunchAlreadyRunning) {
		t.Fatalf("second launch error = %v, want typed already-running conflict", err)
	}
	if starts.Load() != 1 {
		t.Fatalf("executor starts = %d, want one active child", starts.Load())
	}
	close(firstWait)
	if err := <-firstResult; err != nil {
		t.Fatalf("first launch result: %v", err)
	}
}

func TestUDSLaunchWaitsBeyondInitialRPCDeadlineAndRequiresPersistenceAck(t *testing.T) {
	var waits atomic.Int32
	executor := launchExecutorFunc(func(_ context.Context, _ LaunchAsset) (LaunchProcess, error) {
		return launchProcessFunc(func() LaunchExit {
			waits.Add(1)
			time.Sleep(2100 * time.Millisecond)
			return launchExit(domain.SessionOutcomeNormalExit, 0)
		}), nil
	})
	client, closeServer := startLaunchServer(t, executor)
	defer closeServer()
	client.Timeout = 100 * time.Millisecond // bounds only the local socket dial, not gameplay.

	startedAt := time.Now()
	ended, err := client.LaunchAsset(context.Background(), validLaunchAsset(), func(started LaunchStarted) bool {
		return started.SessionID == validLaunchAsset().SessionID
	})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(startedAt) < 2*time.Second || waits.Load() != 1 || ended.Outcome != domain.SessionOutcomeNormalExit {
		t.Fatalf("long launch returned early or incorrectly: elapsed=%s waits=%d ended=%#v", time.Since(startedAt), waits.Load(), ended)
	}
}

func TestUDSLaunchDoesNotMonitorChildUntilSessionPersistenceIsAcknowledged(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	executor := newHelperLaunchExecutor(t, pidPath)
	client, closeServer := startLaunchServer(t, executor)
	defer closeServer()
	ended, err := client.LaunchAsset(context.Background(), validLaunchAsset(), func(LaunchStarted) bool {
		return false
	})
	if !errors.Is(err, ErrLaunchNotPersisted) {
		t.Fatalf("unpersisted launch result = %#v, %v", ended, err)
	}
	pid := waitForPID(t, pidPath)
	select {
	case <-executor.waited:
	case <-time.After(3 * time.Second):
		t.Fatal("Agent did not wait/reap the unpersisted child before return")
	}
	if ended.Outcome != domain.SessionOutcomeInterrupted {
		t.Fatalf("unpersisted child terminal event = %#v, want interrupted", ended)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("unpersisted helper process %d still exists after Agent reaped it: %v", pid, err)
	}
}

func TestUDSLaunchMissingPersistenceAcknowledgementCancelsAndReapsChild(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	executor := newHelperLaunchExecutor(t, pidPath)
	client, closeServer := startLaunchServer(t, executor)
	defer closeServer()
	conn, err := net.DialTimeout("unix", client.SocketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(Request{Operation: OperationLaunchAsset, LaunchAsset: pointer(validLaunchAsset())}); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(conn)
	var started Response
	if err := decoder.Decode(&started); err != nil {
		t.Fatal(err)
	}
	if started.LaunchStarted == nil || started.LaunchStarted.ValidateFor(validLaunchAsset()) != nil {
		t.Fatalf("initial launch response = %#v, want valid started event", started)
	}
	pid := waitForPID(t, pidPath)
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	var terminal Response
	if err := decoder.Decode(&terminal); err != nil {
		t.Fatalf("read launch after missing persistence acknowledgement: %v", err)
	}
	if terminal.LaunchEnded == nil || terminal.LaunchEnded.Outcome != domain.SessionOutcomeInterrupted {
		t.Fatalf("missing-ack terminal event = %#v, want interrupted after child cancellation", terminal)
	}
	select {
	case <-executor.waited:
	case <-time.After(time.Second):
		t.Fatal("Agent did not reap child after persistence acknowledgement timeout")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("helper process %d still exists after acknowledgement timeout cleanup: %v", pid, err)
	}
}

func TestUDSLaunchDisconnectCancelsAndReapsOnlyItsHelperChild(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	executor := newHelperLaunchExecutor(t, pidPath)
	client, closeServer := startLaunchServer(t, executor)
	defer closeServer()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.LaunchAsset(ctx, validLaunchAsset(), func(LaunchStarted) bool { return true })
		result <- err
	}()
	pid := waitForPID(t, pidPath)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled launch client error = %v, want context.Canceled", err)
	}
	select {
	case <-executor.waited:
	case <-time.After(3 * time.Second):
		t.Fatal("Agent did not wait/reap the disconnected child")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("helper process %d still exists after Agent reaped it: %v", pid, err)
	}
}

func TestUDSLaunchHelperProcess(t *testing.T) {
	if os.Getenv("LERNAE_AGENT_LAUNCH_HELPER") != "1" {
		return
	}
	if err := os.WriteFile(os.Getenv("LERNAE_AGENT_LAUNCH_PID_FILE"), []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		os.Exit(3)
	}
	for {
		time.Sleep(time.Second)
	}
}

type launchExecutorFunc func(context.Context, LaunchAsset) (LaunchProcess, error)

func (executor launchExecutorFunc) Start(ctx context.Context, request LaunchAsset) (LaunchProcess, error) {
	return executor(ctx, request)
}

type launchProcessFunc func() LaunchExit

func (process launchProcessFunc) Wait() LaunchExit { return process() }

type helperLaunchExecutor struct {
	pidPath string
	waited  chan struct{}
}

func newHelperLaunchExecutor(t *testing.T, pidPath string) *helperLaunchExecutor {
	t.Helper()
	return &helperLaunchExecutor{pidPath: pidPath, waited: make(chan struct{})}
}

func (executor *helperLaunchExecutor) Start(ctx context.Context, _ LaunchAsset) (LaunchProcess, error) {
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUDSLaunchHelperProcess$")
	command.Env = append(os.Environ(), "LERNAE_AGENT_LAUNCH_HELPER=1", "LERNAE_AGENT_LAUNCH_PID_FILE="+executor.pidPath)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return nil, err
	}
	if _, err := waitForPIDFile(executor.pidPath); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, err
	}
	return launchProcessFunc(func() LaunchExit {
		defer close(executor.waited)
		err := command.Wait()
		if ctx.Err() != nil {
			return LaunchExit{Outcome: domain.SessionOutcomeInterrupted}
		}
		if err != nil {
			return LaunchExit{Outcome: domain.SessionOutcomeInterrupted}
		}
		return launchExit(domain.SessionOutcomeNormalExit, 0)
	}), nil
}

func startLaunchServer(t *testing.T, executor LaunchExecutor) (UDSClient, func()) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	server, err := ListenUDSWithExecutors(socketPath, nil, executor)
	if err != nil {
		t.Fatal(err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve() }()
	return UDSClient{SocketPath: socketPath, Timeout: time.Second}, func() {
		if err := server.Close(); err != nil {
			t.Errorf("close Agent server: %v", err)
		}
		if err := <-serveResult; err != nil {
			t.Errorf("Agent Serve() after close: %v", err)
		}
	}
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	pid, err := waitForPIDFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func waitForPIDFile(path string) (int, error) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if _, err := fmt.Sscan(string(contents), &pid); err == nil && pid > 0 {
				return pid, nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, fmt.Errorf("helper process did not write PID file %s", path)
}

func launchExit(outcome domain.SessionOutcome, code int) LaunchExit {
	return LaunchExit{Outcome: outcome, ExitCode: intPointer(code)}
}

func intPointer(value int) *int { return &value }

func sameIntPointer(left, right *int) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
