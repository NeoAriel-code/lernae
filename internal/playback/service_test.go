package playback_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"lernae/internal/agent"
	"lernae/internal/catalog"
	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/playback"
	"lernae/internal/sessions"
)

func TestLaunchPersistsAfterAgentStartAndFinalizesOnExit(t *testing.T) {
	db, sessionService := openPlaybackDatabase(t, true)
	executor := &sqliteCheckingExecutor{sessions: sessionService, outcome: domain.SessionOutcomeNonZeroExit, exitCode: 7, persisted: make(chan bool, 1)}
	client, closeAgent := startPlaybackAgent(t, executor)
	defer closeAgent()

	service, err := playback.NewService(context.Background(), client, sessionService)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	ended, err := service.Launch(validPlaybackRequest())
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	select {
	case persisted := <-executor.persisted:
		if !persisted {
			t.Fatal("Agent waited for its process before the active Session was durably persisted")
		}
	case <-time.After(time.Second):
		t.Fatal("Agent did not wait for its process after sending the start acknowledgement")
	}
	if ended.SessionID == "" || ended.Outcome != domain.SessionOutcomeNonZeroExit || ended.ExitCode == nil || *ended.ExitCode != 7 {
		t.Fatalf("Launch() terminal event = %#v", ended)
	}
	loaded, err := sessionService.Get(context.Background(), ended.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EndedAt == nil || loaded.Outcome != domain.SessionOutcomeNonZeroExit || !loaded.EndedAt.Equal(ended.EndedAt) {
		t.Fatalf("persisted Session lifecycle = %#v", loaded)
	}
	if err := sessionService.Finish(context.Background(), loaded.ID, domain.SessionOutcomeNormalExit, ended.EndedAt.Add(time.Second)); !errors.Is(err, sessions.ErrInvalidTransition) {
		t.Fatalf("second terminal write error = %v, want ErrInvalidTransition", err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM sessions WHERE id = ?", loaded.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("persisted Session row count = %d, %v; want 1 row", count, err)
	}
}

func TestFailedAgentStartLeavesNoSessionRow(t *testing.T) {
	_, sessionService := openPlaybackDatabase(t, true)
	requestIDs := make(chan domain.SessionID, 1)
	executor := launchExecutorFunc(func(_ context.Context, request agent.LaunchAsset) (agent.LaunchProcess, error) {
		requestIDs <- request.SessionID
		return nil, errors.New("synthetic process start failure")
	})
	client, closeAgent := startPlaybackAgent(t, executor)
	defer closeAgent()
	service, err := playback.NewService(context.Background(), client, sessionService)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	_, err = service.Launch(validPlaybackRequest())
	if !errors.Is(err, agent.ErrLaunchStartFailed) {
		t.Fatalf("Launch() error = %v, want safe launch failure", err)
	}
	id := <-requestIDs
	if _, err := sessionService.Get(context.Background(), id); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("failed-start Session lookup error = %v, want ErrNotFound", err)
	}
}

func TestLocalCacheInvalidPreStartLeavesNoSessionRow(t *testing.T) {
	_, sessionService := openPlaybackDatabase(t, true)
	requestIDs := make(chan domain.SessionID, 1)
	executor := launchExecutorFunc(func(_ context.Context, request agent.LaunchAsset) (agent.LaunchProcess, error) {
		requestIDs <- request.SessionID
		return nil, agent.ErrLaunchLocalCacheInvalid
	})
	client, closeAgent := startPlaybackAgent(t, executor)
	defer closeAgent()
	service, err := playback.NewService(context.Background(), client, sessionService)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	_, err = service.Launch(validPlaybackRequest())
	if !errors.Is(err, agent.ErrLaunchLocalCacheInvalid) || errors.Is(err, agent.ErrLaunchStartFailed) {
		t.Fatalf("Launch() error = %v, want distinct ErrLaunchLocalCacheInvalid", err)
	}
	id := <-requestIDs
	if _, err := sessionService.Get(context.Background(), id); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("invalid-cache Session lookup error = %v, want ErrNotFound", err)
	}
}

func TestStartedNotificationRunsAfterPersistenceAndCanRejectAgentAcknowledgement(t *testing.T) {
	_, sessionService := openPlaybackDatabase(t, true)
	process := newCancelAwareProcess()
	executor := launchExecutorFunc(func(ctx context.Context, request agent.LaunchAsset) (agent.LaunchProcess, error) {
		process.ctx = ctx
		process.request = request
		return process, nil
	})
	client, closeAgent := startPlaybackAgent(t, executor)
	defer closeAgent()
	service, err := playback.NewService(context.Background(), client, sessionService)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	var notified domain.Session
	ended, err := service.LaunchWithStarted(validPlaybackRequest(), func(session domain.Session) bool {
		notified = session
		persisted, getErr := sessionService.Get(context.Background(), session.ID)
		if getErr != nil {
			t.Errorf("start notification ran before Session persistence: %v", getErr)
			return false
		}
		if persisted.EndedAt != nil || persisted.Outcome != "" {
			t.Errorf("Session was not active at start notification: %#v", persisted)
			return false
		}
		return false
	})
	if !errors.Is(err, playback.ErrSessionStartNotificationRejected) || !errors.Is(err, agent.ErrLaunchNotPersisted) {
		t.Fatalf("LaunchWithStarted() error = %v, want notification rejection and Agent ACK rejection", err)
	}
	if notified.ID == "" || ended.Outcome != domain.SessionOutcomeInterrupted {
		t.Fatalf("notification/terminal evidence = %#v / %#v, want persisted start and interrupted stop", notified, ended)
	}
	select {
	case <-process.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Agent did not reap its process after notification rejection")
	}
	if process.ctx.Err() == nil {
		t.Fatal("Agent process remained active after start notification rejection")
	}
	persisted, err := sessionService.Get(context.Background(), notified.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.EndedAt == nil || persisted.Outcome != domain.SessionOutcomeInterrupted {
		t.Fatalf("rejected-start Session = %#v, want finalized interrupted", persisted)
	}
}

func TestPersistenceFailureRejectsAckAndReapsChild(t *testing.T) {
	_, sessionService := openPlaybackDatabase(t, true)
	process := newCancelAwareProcess()
	executor := launchExecutorFunc(func(ctx context.Context, request agent.LaunchAsset) (agent.LaunchProcess, error) {
		process.ctx = ctx
		process.request = request
		return process, nil
	})
	client, closeAgent := startPlaybackAgent(t, executor)
	defer closeAgent()
	service, err := playback.NewService(context.Background(), client, sessionService)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	request := validPlaybackRequest()
	request.WorkID = "missing-work"
	_, err = service.Launch(request)
	if !errors.Is(err, agent.ErrLaunchNotPersisted) {
		t.Fatalf("Launch() error = %v, want safe persistence rejection", err)
	}
	select {
	case <-process.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Agent did not wait/reap child after persistence rejection")
	}
	if process.ctx.Err() == nil {
		t.Fatal("Agent child context remained active after persistence rejection")
	}
	if _, err := sessionService.Get(context.Background(), process.request.SessionID); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("persistence-failed Session lookup error = %v, want ErrNotFound", err)
	}
}

func TestServiceCloseCancelsLaunchAndWaitsForAgentReap(t *testing.T) {
	_, sessionService := openPlaybackDatabase(t, true)
	process := newCancelAwareProcess()
	executor := launchExecutorFunc(func(ctx context.Context, request agent.LaunchAsset) (agent.LaunchProcess, error) {
		process.ctx = ctx
		process.request = request
		return process, nil
	})
	client, closeAgent := startPlaybackAgent(t, executor)
	defer closeAgent()
	parent, cancelParent := context.WithCancel(context.Background())
	service, err := playback.NewService(parent, client, sessionService)
	if err != nil {
		t.Fatal(err)
	}
	launched := make(chan error, 1)
	go func() { _, err := service.Launch(validPlaybackRequest()); launched <- err }()
	select {
	case <-process.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Agent did not start fake child")
	}
	cancelParent()
	if err := service.Close(); err != nil {
		t.Fatalf("Service.Close() = %v", err)
	}
	select {
	case <-process.done:
	default:
		t.Fatal("Service.Close() returned before Agent reaped child")
	}
	if err := <-launched; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Launch() error = %v, want context.Canceled", err)
	}
	loaded, err := sessionService.Get(context.Background(), process.request.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EndedAt == nil || loaded.Outcome != domain.SessionOutcomeInterrupted {
		t.Fatalf("cancelled Session lifecycle = %#v, want finalized interrupted", loaded)
	}
}

func TestServiceCloseWaitsForAgentProcessReapAfterCancellation(t *testing.T) {
	_, sessionService := openPlaybackDatabase(t, true)
	process := newReapBlockingProcess()
	executor := launchExecutorFunc(func(ctx context.Context, request agent.LaunchAsset) (agent.LaunchProcess, error) {
		process.ctx = ctx
		process.request = request
		return process, nil
	})
	client, closeAgent := startPlaybackAgent(t, executor)
	defer closeAgent()
	service, err := playback.NewService(context.Background(), client, sessionService)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		process.releaseReap()
		_ = service.Close()
	}()
	launched := make(chan error, 1)
	go func() { _, err := service.Launch(validPlaybackRequest()); launched <- err }()
	select {
	case <-process.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("Agent did not enter process Wait")
	}

	closed := make(chan error, 1)
	go func() { closed <- service.Close() }()
	select {
	case <-process.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("Agent did not observe Server cancellation")
	}
	select {
	case err := <-closed:
		t.Fatalf("Service.Close() returned before fake Agent process reap: %v", err)
	case <-time.After(2300 * time.Millisecond):
	}

	process.releaseReap()
	select {
	case <-process.reaped:
	case <-time.After(time.Second):
		t.Fatal("fake process did not confirm reap after release")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Service.Close() after reap = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Service.Close() did not finish after Agent reap confirmation")
	}
	if err := <-launched; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Launch() error = %v, want context.Canceled", err)
	}
	loaded, err := sessionService.Get(context.Background(), process.request.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EndedAt == nil || loaded.Outcome != domain.SessionOutcomeInterrupted {
		t.Fatalf("cancelled Session lifecycle = %#v, want finalized interrupted", loaded)
	}
}

func TestServiceCloseDrainsTerminalAfterPersistenceAckWriteFails(t *testing.T) {
	_, sessionService := openPlaybackDatabase(t, true)
	store := newGatedSessionStore(sessionService)
	process := newTerminalGatedProcess()
	executor := launchExecutorFunc(func(ctx context.Context, _ agent.LaunchAsset) (agent.LaunchProcess, error) {
		process.ctx = ctx
		return process, nil
	})
	client, closeAgent := startPlaybackAgent(t, executor)
	defer closeAgent()
	parent, cancelParent := context.WithCancel(context.Background())
	service, err := playback.NewService(parent, client, store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		store.release()
		process.release()
		cancelParent()
		_ = service.Close()
	}()

	type launchResult struct {
		ended agent.LaunchEnded
		err   error
	}
	launched := make(chan launchResult, 1)
	go func() {
		ended, err := service.Launch(validPlaybackRequest())
		launched <- launchResult{ended: ended, err: err}
	}()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Agent did not send launch-start event into Session persistence")
	}

	closed := make(chan error, 1)
	go func() { closed <- service.Close() }()
	select {
	case <-process.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("Agent did not enter Process.Wait after client cancellation")
	}
	select {
	case <-process.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("Agent did not cancel its child after client half-close")
	}

	store.release()
	select {
	case err := <-store.persisted:
		if err != nil {
			t.Fatalf("gated StartWithID persistence failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Session persistence callback did not complete")
	}
	select {
	case err := <-closed:
		t.Fatalf("Service.Close() returned before terminal-after-Wait proof: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	process.release()
	select {
	case <-process.reaped:
	case <-time.After(2 * time.Second):
		t.Fatal("fake Agent process did not complete Wait")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Service.Close() after terminal event = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Service.Close() did not complete after terminal event")
	}
	result := <-launched
	if result.ended.SessionID == "" || result.ended.Outcome != domain.SessionOutcomeInterrupted {
		t.Fatalf("Launch() did not return the Agent terminal event: %#v (err %v)", result.ended, result.err)
	}
	loaded, err := sessionService.Get(context.Background(), result.ended.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EndedAt == nil || loaded.Outcome != domain.SessionOutcomeInterrupted {
		t.Fatalf("persisted Session = %#v, want terminal interruption", loaded)
	}
}

func TestLostTerminalStreamLeavesActiveSessionAndCloseError(t *testing.T) {
	_, sessionService := openPlaybackDatabase(t, true)
	client, waitAgent := startAgentThatDropsBeforeTerminal(t)
	defer waitAgent()
	service, err := playback.NewService(context.Background(), client, sessionService)
	if err != nil {
		t.Fatal(err)
	}
	ended, launchErr := service.Launch(validPlaybackRequest())
	if !errors.Is(launchErr, playback.ErrTerminalUnconfirmed) {
		t.Fatalf("Launch() error = %v, want ErrTerminalUnconfirmed", launchErr)
	}
	closeErr := service.Close()
	if !errors.Is(closeErr, playback.ErrTerminalUnconfirmed) {
		t.Fatalf("Service.Close() error = %v, want ErrTerminalUnconfirmed", closeErr)
	}
	if ended.SessionID == "" {
		t.Fatal("Launch() omitted the Session identity needed for startup reconciliation")
	}
	loaded, err := sessionService.Get(context.Background(), ended.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EndedAt != nil || loaded.Outcome != "" {
		t.Fatalf("transport loss fabricated terminal Session evidence: %#v", loaded)
	}
}

func TestAcceptedLaunchWithoutStartedEventLeavesUnconfirmedShutdownState(t *testing.T) {
	db, sessionService := openPlaybackDatabase(t, true)
	client, receivedRequest, childStarted, childReaped, waitAgent := startAgentThatDropsBeforeStarted(t)
	defer waitAgent()
	service, err := playback.NewService(context.Background(), client, sessionService)
	if err != nil {
		t.Fatal(err)
	}

	ended, launchErr := service.Launch(validPlaybackRequest())
	if !errors.Is(launchErr, playback.ErrTerminalUnconfirmed) {
		t.Fatalf("Launch() error = %v, want ErrTerminalUnconfirmed after dispatched request", launchErr)
	}
	closeErr := service.Close()
	if !errors.Is(closeErr, playback.ErrTerminalUnconfirmed) {
		t.Fatalf("Service.Close() error = %v, want ErrTerminalUnconfirmed", closeErr)
	}
	request := <-receivedRequest
	select {
	case <-childStarted:
	case <-time.After(time.Second):
		t.Fatal("fake Agent did not start its child after accepting LaunchAsset")
	}
	select {
	case <-childReaped:
	case <-time.After(time.Second):
		t.Fatal("fake Agent did not cancel and reap the child after its launch stream closed")
	}
	if ended.SessionID != request.SessionID || !ended.EndedAt.IsZero() || ended.Outcome != "" || ended.ExitCode != nil {
		t.Fatalf("Launch() fabricated terminal evidence or lost request correlation: %#v", ended)
	}
	if _, err := sessionService.Get(context.Background(), request.SessionID); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Session lookup = %v, want no active row without confirmed start", err)
	}
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM sessions WHERE id = ?", request.SessionID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("Session rows for unconfirmed start = %d, %v; want zero", rows, err)
	}
}

func TestLaunchDialFailureIsCertainNotDispatched(t *testing.T) {
	db, sessionService := openPlaybackDatabase(t, true)
	service, err := playback.NewService(context.Background(), agent.UDSClient{
		SocketPath: filepath.Join(t.TempDir(), "missing-agent.sock"), Timeout: 100 * time.Millisecond,
	}, sessionService)
	if err != nil {
		t.Fatal(err)
	}
	_, launchErr := service.Launch(validPlaybackRequest())
	if launchErr == nil || errors.Is(launchErr, playback.ErrTerminalUnconfirmed) {
		t.Fatalf("pre-dispatch dial error = %v, want a certain connection failure", launchErr)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Service.Close() after failed dial = %v, want nil", err)
	}
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("Session rows after failed dial = %d, %v; want zero", rows, err)
	}
}

func TestLaunchPersistsValidTerminalOutcomeWhenCancellationRacesAfterEvent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outcome  domain.SessionOutcome
		exitCode int
	}{
		{name: "normal exit", outcome: domain.SessionOutcomeNormalExit, exitCode: 0},
		{name: "non-zero exit", outcome: domain.SessionOutcomeNonZeroExit, exitCode: 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, sessionService := openPlaybackDatabase(t, true)
			parent, cancelParent := context.WithCancel(context.Background())
			client := newTerminalRaceClient(tc.outcome, tc.exitCode)
			service, err := playback.NewService(parent, client, sessionService)
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			defer cancelParent()
			defer client.release()
			launched := make(chan error, 1)
			go func() { _, err := service.Launch(validPlaybackRequest()); launched <- err }()

			select {
			case <-client.terminalReceived:
			case <-time.After(2 * time.Second):
				t.Fatal("fake UDS client did not receive terminal event")
			}
			cancelParent()
			client.release()
			if err := <-launched; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled Launch() error = %v, want context.Canceled", err)
			}
			loaded, err := sessionService.Get(context.Background(), client.request.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.EndedAt == nil || loaded.Outcome != tc.outcome {
				t.Fatalf("persisted terminal Session = %#v, want %q", loaded, tc.outcome)
			}
		})
	}
}

func TestLaunchPersistsInterruptedAfterCancellationTerminalEvent(t *testing.T) {
	_, sessionService := openPlaybackDatabase(t, true)
	parent, cancelParent := context.WithCancel(context.Background())
	client := &cancelBeforeTerminalClient{started: make(chan struct{})}
	service, err := playback.NewService(parent, client, sessionService)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	launched := make(chan error, 1)
	go func() { _, err := service.Launch(validPlaybackRequest()); launched <- err }()
	select {
	case <-client.started:
	case <-time.After(2 * time.Second):
		t.Fatal("fake UDS client did not confirm start")
	}
	cancelParent()
	if err := <-launched; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Launch() error = %v, want context.Canceled", err)
	}
	loaded, err := sessionService.Get(context.Background(), client.request.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EndedAt == nil || loaded.Outcome != domain.SessionOutcomeInterrupted {
		t.Fatalf("cancelled Session lifecycle = %#v, want finalized interrupted", loaded)
	}
}

type launchExecutorFunc func(context.Context, agent.LaunchAsset) (agent.LaunchProcess, error)

func (executor launchExecutorFunc) Start(ctx context.Context, request agent.LaunchAsset) (agent.LaunchProcess, error) {
	return executor(ctx, request)
}

type gatedSessionStore struct {
	delegate  playback.SessionStore
	entered   chan struct{}
	allow     chan struct{}
	persisted chan error
	once      sync.Once
}

func newGatedSessionStore(delegate playback.SessionStore) *gatedSessionStore {
	return &gatedSessionStore{
		delegate: delegate, entered: make(chan struct{}), allow: make(chan struct{}),
		persisted: make(chan error, 1),
	}
}

func (store *gatedSessionStore) NewID() (domain.SessionID, error) { return store.delegate.NewID() }

func (store *gatedSessionStore) StartWithID(_ context.Context, id domain.SessionID, work domain.WorkID, edition domain.EditionID, asset domain.AssetID, startedAt time.Time) (domain.Session, error) {
	close(store.entered)
	<-store.allow
	session, err := store.delegate.StartWithID(context.Background(), id, work, edition, asset, startedAt)
	store.persisted <- err
	return session, err
}

func (store *gatedSessionStore) Finish(ctx context.Context, id domain.SessionID, outcome domain.SessionOutcome, endedAt time.Time) error {
	return store.delegate.Finish(ctx, id, outcome, endedAt)
}

func (store *gatedSessionStore) release() { store.once.Do(func() { close(store.allow) }) }

type terminalGatedProcess struct {
	ctx         context.Context
	waiting     chan struct{}
	cancelled   chan struct{}
	allow       chan struct{}
	reaped      chan struct{}
	once        sync.Once
	releaseOnce sync.Once
}

func newTerminalGatedProcess() *terminalGatedProcess {
	return &terminalGatedProcess{
		waiting: make(chan struct{}), cancelled: make(chan struct{}),
		allow: make(chan struct{}), reaped: make(chan struct{}),
	}
}

func (process *terminalGatedProcess) Wait() agent.LaunchExit {
	process.once.Do(func() { close(process.waiting) })
	<-process.ctx.Done()
	close(process.cancelled)
	<-process.allow
	close(process.reaped)
	return agent.LaunchExit{Outcome: domain.SessionOutcomeInterrupted}
}

func (process *terminalGatedProcess) release() {
	process.releaseOnce.Do(func() { close(process.allow) })
}

func startAgentThatDropsBeforeTerminal(t *testing.T) (agent.UDSClient, func()) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "drop-agent.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			served <- err
			return
		}
		defer conn.Close()
		decoder := json.NewDecoder(conn)
		var request agent.Request
		if err := decoder.Decode(&request); err != nil || request.LaunchAsset == nil {
			served <- errors.New("fake Agent did not receive typed launch request")
			return
		}
		started := agent.LaunchStarted{
			SessionID: request.LaunchAsset.SessionID, WorkID: request.LaunchAsset.WorkID,
			EditionID: request.LaunchAsset.EditionID, AssetID: request.LaunchAsset.AssetID, StartedAt: time.Now().UTC(),
		}
		if err := json.NewEncoder(conn).Encode(agent.Response{LaunchStarted: &started}); err != nil {
			served <- err
			return
		}
		var acknowledgement agent.LaunchAcknowledgement
		if err := decoder.Decode(&acknowledgement); err != nil || acknowledgement.SessionID != started.SessionID || !acknowledgement.Persisted {
			served <- errors.New("fake Agent did not receive durable Session acknowledgement")
			return
		}
		// Drop transport without a terminal-after-Wait event: no reap proof exists.
		served <- nil
	}()
	return agent.UDSClient{SocketPath: socketPath, Timeout: time.Second}, func() {
		_ = listener.Close()
		if err := <-served; err != nil {
			t.Errorf("fake drop Agent: %v", err)
		}
	}
}

func startAgentThatDropsBeforeStarted(t *testing.T) (agent.UDSClient, <-chan agent.LaunchAsset, <-chan struct{}, <-chan struct{}, func()) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "s")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan agent.LaunchAsset, 1)
	childStarted := make(chan struct{})
	reaped := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			served <- err
			return
		}
		decoder := json.NewDecoder(conn)
		var request agent.Request
		if err := decoder.Decode(&request); err != nil || request.LaunchAsset == nil {
			_ = conn.Close()
			served <- errors.New("fake Agent did not receive typed LaunchAsset request")
			return
		}
		received <- *request.LaunchAsset
		childCtx, cancelChild := context.WithCancel(context.Background())
		close(childStarted)
		go func() {
			<-childCtx.Done()
			close(reaped)
		}()
		// Model the Agent having started its owned child but losing the stream
		// before it can publish LaunchStarted. Its established failure path
		// cancels and reaps the child, but the client has no terminal proof.
		_ = conn.Close()
		cancelChild()
		<-reaped
		served <- nil
	}()
	return agent.UDSClient{SocketPath: socketPath, Timeout: time.Second}, received, childStarted, reaped, func() {
		_ = listener.Close()
		if err := <-served; err != nil {
			t.Errorf("fake pre-start drop Agent: %v", err)
		}
	}
}

type terminalRaceClient struct {
	outcome          domain.SessionOutcome
	exitCode         int
	request          agent.LaunchAsset
	terminalReceived chan struct{}
	returnNow        chan struct{}
	releaseOnce      sync.Once
}

func (client *terminalRaceClient) release() {
	client.releaseOnce.Do(func() { close(client.returnNow) })
}

func newTerminalRaceClient(outcome domain.SessionOutcome, exitCode int) *terminalRaceClient {
	return &terminalRaceClient{
		outcome: outcome, exitCode: exitCode,
		terminalReceived: make(chan struct{}), returnNow: make(chan struct{}),
	}
}

func (client *terminalRaceClient) LaunchAsset(ctx context.Context, request agent.LaunchAsset, onStarted func(agent.LaunchStarted) bool) (agent.LaunchEnded, error) {
	client.request = request
	startedAt := time.Now().UTC()
	started := agent.LaunchStarted{
		SessionID: request.SessionID, WorkID: request.WorkID, EditionID: request.EditionID,
		AssetID: request.AssetID, StartedAt: startedAt,
	}
	if !onStarted(started) {
		return agent.LaunchEnded{}, agent.ErrLaunchNotPersisted
	}
	code := client.exitCode
	ended := agent.LaunchEnded{
		SessionID: request.SessionID, WorkID: request.WorkID, EditionID: request.EditionID,
		AssetID: request.AssetID, EndedAt: startedAt.Add(time.Second), Outcome: client.outcome,
		ExitCode: &code,
	}
	close(client.terminalReceived)
	<-client.returnNow
	return ended, ctx.Err()
}

type cancelBeforeTerminalClient struct {
	request   agent.LaunchAsset
	startedAt time.Time
	started   chan struct{}
}

func (client *cancelBeforeTerminalClient) LaunchAsset(ctx context.Context, request agent.LaunchAsset, onStarted func(agent.LaunchStarted) bool) (agent.LaunchEnded, error) {
	client.request = request
	client.startedAt = time.Now().UTC()
	if !onStarted(agent.LaunchStarted{
		SessionID: request.SessionID, WorkID: request.WorkID, EditionID: request.EditionID,
		AssetID: request.AssetID, StartedAt: client.startedAt,
	}) {
		return agent.LaunchEnded{}, agent.ErrLaunchNotPersisted
	}
	close(client.started)
	<-ctx.Done()
	return agent.LaunchEnded{
		SessionID: request.SessionID, WorkID: request.WorkID, EditionID: request.EditionID,
		AssetID: request.AssetID, EndedAt: time.Now().UTC(), Outcome: domain.SessionOutcomeInterrupted,
	}, ctx.Err()
}

type sqliteCheckingExecutor struct {
	sessions  *sessions.Service
	outcome   domain.SessionOutcome
	exitCode  int
	persisted chan bool
}

func (executor *sqliteCheckingExecutor) Start(_ context.Context, request agent.LaunchAsset) (agent.LaunchProcess, error) {
	return launchProcessFunc(func() agent.LaunchExit {
		loaded, err := executor.sessions.Get(context.Background(), request.SessionID)
		executor.persisted <- err == nil && loaded.EndedAt == nil && loaded.Outcome == ""
		code := executor.exitCode
		return agent.LaunchExit{Outcome: executor.outcome, ExitCode: &code}
	}), nil
}

type launchProcessFunc func() agent.LaunchExit

func (process launchProcessFunc) Wait() agent.LaunchExit { return process() }

type cancelAwareProcess struct {
	ctx     context.Context
	request agent.LaunchAsset
	started chan struct{}
	done    chan struct{}
	once    sync.Once
}

type reapBlockingProcess struct {
	ctx       context.Context
	request   agent.LaunchAsset
	waiting   chan struct{}
	cancelled chan struct{}
	allowReap chan struct{}
	reaped    chan struct{}
	once      sync.Once
}

func newReapBlockingProcess() *reapBlockingProcess {
	return &reapBlockingProcess{
		waiting: make(chan struct{}), cancelled: make(chan struct{}),
		allowReap: make(chan struct{}), reaped: make(chan struct{}),
	}
}

func (process *reapBlockingProcess) Wait() agent.LaunchExit {
	process.once.Do(func() { close(process.waiting) })
	<-process.ctx.Done()
	close(process.cancelled)
	<-process.allowReap
	close(process.reaped)
	return agent.LaunchExit{Outcome: domain.SessionOutcomeInterrupted}
}

func (process *reapBlockingProcess) releaseReap() {
	select {
	case <-process.allowReap:
	default:
		close(process.allowReap)
	}
}

func newCancelAwareProcess() *cancelAwareProcess {
	return &cancelAwareProcess{started: make(chan struct{}), done: make(chan struct{})}
}

func (process *cancelAwareProcess) Wait() agent.LaunchExit {
	process.once.Do(func() { close(process.started) })
	<-process.ctx.Done()
	close(process.done)
	return agent.LaunchExit{Outcome: domain.SessionOutcomeInterrupted}
}

func openPlaybackDatabase(t *testing.T, seed bool) (*sql.DB, *sessions.Service) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "playback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if seed {
		graph := catalog.WorkGraph{
			Work:     domain.Work{ID: "work-1", Medium: domain.MediumGame, WorkType: "game", Title: "Test Game"},
			Editions: []domain.Edition{{ID: "edition-1", WorkID: "work-1", Platform: "gamecube", Format: "disc_image"}},
			Assets:   []domain.Asset{{ID: "asset-1", EditionID: "edition-1", Kind: "disc_image"}},
		}
		if err := catalog.NewSQLiteRepository(db).CreateGraph(context.Background(), graph); err != nil {
			t.Fatal(err)
		}
	}
	return db, sessions.NewService(sessions.NewSQLiteRepository(db))
}

func startPlaybackAgent(t *testing.T, executor agent.LaunchExecutor) (agent.UDSClient, func()) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	server, err := agent.ListenUDSWithExecutors(socketPath, nil, executor)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve() }()
	return agent.UDSClient{SocketPath: socketPath, Timeout: time.Second}, func() {
		if err := server.Close(); err != nil {
			t.Errorf("close fake Agent: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("fake Agent Serve() = %v", err)
		}
	}
}

func validPlaybackRequest() agent.LaunchAsset {
	return agent.LaunchAsset{
		WorkID: "work-1", EditionID: "edition-1", AssetID: "asset-1", PartID: "part-1",
		Medium: domain.MediumGame, Platform: "gamecube", Format: "disc_image", Role: "rom",
		Filename: "game.iso", ExpectedBytes: 4,
	}
}
