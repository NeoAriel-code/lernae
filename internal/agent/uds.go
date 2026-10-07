package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"lernae/internal/domain"
)

const (
	maxProtocolMessageBytes     = 64 * 1024
	cancelCleanupAckTimeout     = 2 * time.Second
	launchPersistenceAckTimeout = 2 * time.Second
)

var ErrUnsupportedOperation = errors.New("unsupported Agent operation")

type UDSClient struct {
	SocketPath string
	Timeout    time.Duration
}

// LaunchAsset holds the UDS connection for the whole local process lifetime.
// timeout only bounds dialing; the caller context owns the operation lifetime.
// onStarted must synchronously persist the active Session and return true only
// after that write succeeds. A missing or false acknowledgement stops the child.
func (client UDSClient) LaunchAsset(
	ctx context.Context,
	request LaunchAsset,
	onStarted func(LaunchStarted) bool,
) (LaunchEnded, error) {
	if ctx == nil {
		return LaunchEnded{}, errors.New("Agent launch requires a context")
	}
	if err := request.Validate(); err != nil {
		return LaunchEnded{}, err
	}
	dialTimeout := client.Timeout
	if dialTimeout <= 0 {
		dialTimeout = 500 * time.Millisecond
	}
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "unix", client.SocketPath)
	if err != nil {
		return LaunchEnded{}, fmt.Errorf("connect to Lernae Agent socket at %s: %w", client.SocketPath, err)
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return LaunchEnded{}, errors.New("Lernae Agent launch requires a Unix Domain Socket")
	}
	cancelMonitorDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// Half-closing lets the Agent observe client cancellation, cancel and
			// reap only its owned child, then return a terminal cleanup event.
			// Do not time out this read: only that event confirms Wait returned.
			_ = unixConn.CloseWrite()
		case <-cancelMonitorDone:
		}
	}()
	defer func() {
		close(cancelMonitorDone)
		_ = unixConn.Close()
	}()
	if err := ctx.Err(); err != nil {
		return LaunchEnded{}, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = unixConn.SetWriteDeadline(deadline)
	}
	if err := json.NewEncoder(unixConn).Encode(Request{Operation: OperationLaunchAsset, LaunchAsset: &request}); err != nil {
		return LaunchEnded{}, launchStateUnconfirmed(fmt.Errorf("send LaunchAsset request to Lernae Agent: %w", err))
	}
	_ = unixConn.SetWriteDeadline(time.Time{})

	scanner := bufio.NewScanner(unixConn)
	scanner.Buffer(make([]byte, 4*1024), maxProtocolMessageBytes)
	readResponse := func() (Response, error) {
		if !scanner.Scan() {
			if ctx.Err() != nil {
				return Response{}, ctx.Err()
			}
			if err := scanner.Err(); err != nil {
				return Response{}, fmt.Errorf("read LaunchAsset response from Lernae Agent: %w", err)
			}
			return Response{}, io.ErrUnexpectedEOF
		}
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		var response Response
		if err := decoder.Decode(&response); err != nil {
			return Response{}, fmt.Errorf("decode LaunchAsset response from Lernae Agent: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return Response{}, errors.New("Lernae Agent returned multiple JSON values in one launch message")
		}
		return response, nil
	}
	response, err := readResponse()
	if err != nil {
		return LaunchEnded{}, launchStateUnconfirmed(err)
	}
	if response.Error != "" {
		return LaunchEnded{}, launchStateUnconfirmed(fmt.Errorf("Lernae Agent rejected LaunchAsset: %s", response.Error))
	}
	if response.Status != nil || response.Progress != nil || response.RestoreResult != nil || response.LaunchEnded != nil ||
		(response.LaunchStarted != nil && response.LaunchFailed != nil) {
		return LaunchEnded{}, launchStateUnconfirmed(errors.New("Lernae Agent returned an invalid LaunchAsset response shape"))
	}
	if response.LaunchFailed != nil {
		if response.LaunchStarted != nil || response.LaunchFailed.ValidateFor(request) != nil {
			return LaunchEnded{}, launchStateUnconfirmed(errors.New("Lernae Agent returned invalid launch failure evidence"))
		}
		switch response.LaunchFailed.Code {
		case LaunchFailureAlreadyRunning:
			return LaunchEnded{}, ErrLaunchAlreadyRunning
		case LaunchFailureLocalCacheInvalid:
			return LaunchEnded{}, ErrLaunchLocalCacheInvalid
		case LaunchFailureStartFailed:
			return LaunchEnded{}, ErrLaunchStartFailed
		default:
			return LaunchEnded{}, launchStateUnconfirmed(errors.New("Lernae Agent returned an unknown launch failure"))
		}
	}
	if response.LaunchStarted == nil || response.LaunchStarted.ValidateFor(request) != nil {
		return LaunchEnded{}, launchStateUnconfirmed(errors.New("Lernae Agent omitted valid launch-start evidence"))
	}
	persisted := onStarted != nil && onStarted(*response.LaunchStarted)
	acknowledgement := LaunchAcknowledgement{SessionID: request.SessionID, Persisted: persisted}
	if deadline, ok := ctx.Deadline(); ok {
		_ = unixConn.SetWriteDeadline(deadline)
	}
	var acknowledgementErr error
	if err := json.NewEncoder(unixConn).Encode(acknowledgement); err != nil {
		acknowledgementErr = fmt.Errorf("acknowledge persisted launch Session to Agent: %w", err)
	}
	_ = unixConn.SetWriteDeadline(time.Time{})

	// Even if cancellation half-closed the request side before this ACK could
	// be written, keep the read side open until Agent confirms Wait completed.
	terminal, err := readResponse()
	if err != nil {
		return LaunchEnded{}, errors.Join(acknowledgementErr, err)
	}
	if terminal.Error != "" || terminal.Status != nil || terminal.Progress != nil || terminal.RestoreResult != nil ||
		terminal.LaunchStarted != nil || terminal.LaunchFailed != nil || terminal.LaunchEnded == nil ||
		terminal.LaunchEnded.ValidateFor(request) != nil {
		return LaunchEnded{}, errors.Join(acknowledgementErr, errors.New("Lernae Agent returned invalid launch terminal evidence"))
	}
	if scanner.Scan() {
		return *terminal.LaunchEnded, errors.Join(acknowledgementErr, errors.New("Lernae Agent returned more than one terminal launch event"))
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return *terminal.LaunchEnded, errors.Join(acknowledgementErr, fmt.Errorf("finish reading LaunchAsset stream from Lernae Agent: %w", err))
	}
	if ctx.Err() != nil {
		return *terminal.LaunchEnded, errors.Join(acknowledgementErr, ctx.Err())
	}
	if !persisted {
		return *terminal.LaunchEnded, errors.Join(acknowledgementErr, ErrLaunchNotPersisted)
	}
	if acknowledgementErr != nil {
		return *terminal.LaunchEnded, acknowledgementErr
	}
	return *terminal.LaunchEnded, nil
}

func launchStateUnconfirmed(err error) error {
	if err == nil {
		return ErrLaunchStateUnconfirmed
	}
	return fmt.Errorf("%w: %w", ErrLaunchStateUnconfirmed, err)
}

func (client UDSClient) GetStatus(ctx context.Context) (AgentStatus, error) {
	timeout := client.Timeout
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", client.SocketPath)
	if err != nil {
		return AgentStatus{}, fmt.Errorf("connect to Lernae Agent socket at %s: %w", client.SocketPath, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if err := json.NewEncoder(conn).Encode(Request{Operation: OperationGetStatus}); err != nil {
		return AgentStatus{}, fmt.Errorf("send GetStatus request to Lernae Agent: %w", err)
	}
	var response Response
	decoder := json.NewDecoder(io.LimitReader(conn, maxProtocolMessageBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return AgentStatus{}, fmt.Errorf("read GetStatus response from Lernae Agent: %w", err)
	}
	if response.Error != "" {
		return AgentStatus{}, fmt.Errorf("Lernae Agent rejected GetStatus: %s", response.Error)
	}
	if response.Status == nil || response.Status.State != StateOnline {
		return AgentStatus{}, errors.New("Lernae Agent returned an invalid GetStatus response")
	}
	return *response.Status, nil
}

// RestoreAsset sends the typed restore operation and consumes bounded NDJSON
// progress messages until the Agent returns one verified final-cache result.
func (client UDSClient) RestoreAsset(ctx context.Context, request RestoreAsset, onProgress func(RestoreProgress)) (RestoreResult, error) {
	if err := request.Validate(); err != nil {
		return RestoreResult{}, err
	}
	conn, err := (&net.Dialer{Timeout: client.Timeout}).DialContext(ctx, "unix", client.SocketPath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("connect to Lernae Agent socket at %s: %w", client.SocketPath, err)
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return RestoreResult{}, errors.New("Lernae Agent restore requires a Unix Domain Socket")
	}
	defer unixConn.Close()
	if err := ctx.Err(); err != nil {
		return RestoreResult{}, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = unixConn.SetWriteDeadline(deadline)
	}
	closeMonitor := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// The caller deadline bounds request writes, not reads. Once the
			// Agent sees EOF, it may still need time to clean its owned staging
			// subtree and acknowledge cancellation.
			_ = unixConn.SetReadDeadline(time.Now().Add(cancelCleanupAckTimeout))
			_ = unixConn.CloseWrite()
		case <-closeMonitor:
		}
	}()
	defer close(closeMonitor)
	if err := json.NewEncoder(unixConn).Encode(Request{Operation: OperationRestoreAsset, RestoreAsset: &request}); err != nil && ctx.Err() == nil {
		return RestoreResult{}, fmt.Errorf("send RestoreAsset request to Lernae Agent: %w", err)
	}
	_ = unixConn.SetWriteDeadline(time.Time{})

	scanner := bufio.NewScanner(unixConn)
	scanner.Buffer(make([]byte, 4*1024), maxProtocolMessageBytes)
	var previous *RestoreProgress
	for scanner.Scan() {
		var response Response
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&response); err != nil {
			if ctx.Err() != nil {
				return RestoreResult{}, ctx.Err()
			}
			return RestoreResult{}, fmt.Errorf("read RestoreAsset response from Lernae Agent: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return RestoreResult{}, errors.New("Lernae Agent returned multiple JSON values in one restore message")
		}
		if response.Error != "" {
			if ctx.Err() != nil {
				return RestoreResult{}, ctx.Err()
			}
			return RestoreResult{}, fmt.Errorf("Lernae Agent rejected RestoreAsset: %s", response.Error)
		}
		if response.Status != nil || (response.Progress != nil && response.RestoreResult != nil) {
			return RestoreResult{}, errors.New("Lernae Agent returned an invalid RestoreAsset response shape")
		}
		if response.Progress != nil {
			if err := ValidateProgressAfter(previous, *response.Progress); err != nil {
				return RestoreResult{}, fmt.Errorf("Lernae Agent returned invalid restore progress: %w", err)
			}
			copy := *response.Progress
			previous = &copy
			if onProgress != nil {
				onProgress(copy)
			}
			continue
		}
		if response.RestoreResult == nil {
			return RestoreResult{}, errors.New("Lernae Agent returned an empty RestoreAsset response")
		}
		if previous == nil || previous.Phase != RestorePhaseComplete || previous.CurrentBytes != previous.TotalBytes {
			return RestoreResult{}, errors.New("Lernae Agent omitted terminal restore progress before local-ready evidence")
		}
		if err := response.RestoreResult.ValidateFor(request); err != nil {
			return RestoreResult{}, fmt.Errorf("Lernae Agent returned invalid local-ready evidence: %w", err)
		}
		return *response.RestoreResult, nil
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return RestoreResult{}, ctx.Err()
		}
		return RestoreResult{}, fmt.Errorf("read RestoreAsset stream from Lernae Agent: %w", err)
	}
	if ctx.Err() != nil {
		return RestoreResult{}, ctx.Err()
	}
	return RestoreResult{}, io.ErrUnexpectedEOF
}

type UDSServer struct {
	listener        *net.UnixListener
	socketPath      string
	socketInfo      os.FileInfo
	startedAt       time.Time
	closeOnce       sync.Once
	closeErr        error
	stateMu         sync.Mutex
	closed          bool
	launchActive    bool
	connections     sync.WaitGroup
	restoreExecutor RestoreExecutor
	launchExecutor  LaunchExecutor
	ctx             context.Context
	cancel          context.CancelFunc
}

func ListenUDS(socketPath string, restoreExecutors ...RestoreExecutor) (*UDSServer, error) {
	if len(restoreExecutors) > 1 {
		return nil, errors.New("ListenUDS accepts at most one restore executor")
	}
	var restoreExecutor RestoreExecutor
	if len(restoreExecutors) == 1 {
		restoreExecutor = restoreExecutors[0]
	}
	return listenUDS(socketPath, restoreExecutor, nil)
}

func ListenUDSWithExecutors(socketPath string, restoreExecutor RestoreExecutor, launchExecutor LaunchExecutor) (*UDSServer, error) {
	return listenUDS(socketPath, restoreExecutor, launchExecutor)
}

func listenUDS(socketPath string, restoreExecutor RestoreExecutor, launchExecutor LaunchExecutor) (*UDSServer, error) {
	if !filepath.IsAbs(socketPath) {
		return nil, fmt.Errorf("Agent socket path must be absolute: %q", socketPath)
	}
	for _, segment := range strings.Split(filepath.ToSlash(socketPath), "/") {
		if segment == ".." {
			return nil, errors.New("Agent socket path must not contain path traversal")
		}
	}
	socketPath = filepath.Clean(socketPath)
	parent := filepath.Dir(socketPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create Agent socket directory: %w", err)
	}
	if err := ensurePrivateDirectory(parent); err != nil {
		return nil, err
	}
	if err := removeStaleSocket(socketPath); err != nil {
		return nil, err
	}
	address := &net.UnixAddr{Name: socketPath, Net: "unix"}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, fmt.Errorf("listen on Lernae Agent socket at %s: %w", socketPath, err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("set Agent socket permissions: %w", err)
	}
	info, err := os.Lstat(socketPath)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("inspect Agent socket after startup: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &UDSServer{
		listener: listener, socketPath: socketPath, socketInfo: info, startedAt: time.Now().UTC(),
		restoreExecutor: restoreExecutor, launchExecutor: launchExecutor, ctx: ctx, cancel: cancel,
	}
	return server, nil
}

func (server *UDSServer) Serve() error {
	for {
		conn, err := server.listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			if temporary, ok := err.(net.Error); ok && temporary.Temporary() {
				time.Sleep(25 * time.Millisecond)
				continue
			}
			return fmt.Errorf("accept Lernae Agent UDS request: %w", err)
		}
		server.stateMu.Lock()
		if server.closed {
			server.stateMu.Unlock()
			_ = conn.Close()
			return nil
		}
		server.connections.Add(1)
		server.stateMu.Unlock()
		go func() {
			defer server.connections.Done()
			server.handle(conn)
		}()
	}
}

func (server *UDSServer) Close() error {
	server.closeOnce.Do(func() {
		server.stateMu.Lock()
		server.closed = true
		server.cancel()
		closeErr := server.listener.Close()
		server.stateMu.Unlock()
		if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			server.closeErr = fmt.Errorf("close Agent UDS listener: %w", closeErr)
		}
		server.connections.Wait()
		current, err := os.Lstat(server.socketPath)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			server.closeErr = fmt.Errorf("inspect Agent socket during shutdown: %w", err)
			return
		}
		if !os.SameFile(server.socketInfo, current) {
			return
		}
		if err := os.Remove(server.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			server.closeErr = fmt.Errorf("remove Agent socket during shutdown: %w", err)
		}
	})
	return server.closeErr
}

func (server *UDSServer) handle(conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReaderSize(conn, 4*1024)
	decoder := json.NewDecoder(io.LimitReader(reader, maxProtocolMessageBytes))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		_ = json.NewEncoder(conn).Encode(Response{Error: "invalid typed request"})
		return
	}
	if err := request.Validate(); err != nil {
		_ = json.NewEncoder(conn).Encode(Response{Error: "invalid typed Agent operation"})
		return
	}
	streamReader := io.MultiReader(decoder.Buffered(), reader)
	if request.Operation == OperationRestoreAsset {
		_ = conn.SetDeadline(time.Time{})
		server.handleRestore(conn, streamReader, *request.RestoreAsset)
		return
	}
	if request.Operation == OperationLaunchAsset {
		_ = conn.SetDeadline(time.Time{})
		server.handleLaunch(conn, streamReader, *request.LaunchAsset)
		return
	}
	status := AgentStatus{State: StateOnline, StartedAt: server.startedAt}
	_ = json.NewEncoder(conn).Encode(Response{Status: &status})
}

func (server *UDSServer) handleRestore(conn *net.UnixConn, requestReader io.Reader, request RestoreAsset) {
	if server.restoreExecutor == nil {
		_ = json.NewEncoder(conn).Encode(Response{Error: "restore executor is not configured"})
		return
	}
	ctx, cancel := context.WithCancel(server.ctx)
	defer cancel()
	go func() {
		monitorUnexpectedInputOrDisconnect(requestReader, cancel)
	}()
	encoder := json.NewEncoder(conn)
	var previous *RestoreProgress
	progress := func(event RestoreProgress) error {
		if err := ValidateProgressAfter(previous, event); err != nil {
			cancel()
			return err
		}
		copy := event
		previous = &copy
		if err := encoder.Encode(Response{Progress: &copy}); err != nil {
			cancel()
			return err
		}
		return nil
	}
	result, err := server.restoreExecutor.Restore(ctx, request, progress)
	if err != nil {
		_ = encoder.Encode(Response{Error: "restore failed"})
		return
	}
	if previous == nil || previous.Phase != RestorePhaseComplete || previous.CurrentBytes != previous.TotalBytes {
		_ = encoder.Encode(Response{Error: "restore omitted terminal progress"})
		return
	}
	if err := result.ValidateFor(request); err != nil {
		_ = encoder.Encode(Response{Error: "restore did not produce verified local-ready evidence"})
		return
	}
	_ = encoder.Encode(Response{RestoreResult: &result})
}

func (server *UDSServer) handleLaunch(conn *net.UnixConn, requestReader io.Reader, request LaunchAsset) {
	encoder := json.NewEncoder(conn)
	if server.launchExecutor == nil {
		_ = encoder.Encode(Response{LaunchFailed: &LaunchFailed{SessionID: request.SessionID, Code: LaunchFailureStartFailed}})
		return
	}
	if failure := server.acquireLaunch(); failure != "" {
		_ = encoder.Encode(Response{LaunchFailed: &LaunchFailed{SessionID: request.SessionID, Code: failure}})
		return
	}
	defer server.releaseLaunch()

	ctx, cancel := context.WithCancel(server.ctx)
	defer cancel()
	stopShutdownClose := context.AfterFunc(server.ctx, func() { _ = conn.Close() })
	defer stopShutdownClose()
	process, err := server.launchExecutor.Start(ctx, request)
	if err != nil || process == nil {
		cancel()
		if process != nil {
			_ = process.Wait()
		}
		failure := LaunchFailureStartFailed
		if process == nil && errors.Is(err, ErrLaunchLocalCacheInvalid) {
			failure = LaunchFailureLocalCacheInvalid
		}
		_ = encoder.Encode(Response{LaunchFailed: &LaunchFailed{SessionID: request.SessionID, Code: failure}})
		return
	}
	started := LaunchStarted{
		SessionID: request.SessionID, WorkID: request.WorkID, EditionID: request.EditionID,
		AssetID: request.AssetID, StartedAt: time.Now().UTC(),
	}
	if err := encoder.Encode(Response{LaunchStarted: &started}); err != nil {
		cancel()
		_ = process.Wait()
		return
	}

	acknowledgementDecoder := json.NewDecoder(io.LimitReader(requestReader, maxProtocolMessageBytes))
	acknowledgementDecoder.DisallowUnknownFields()
	var acknowledgement LaunchAcknowledgement
	_ = conn.SetReadDeadline(time.Now().Add(launchPersistenceAckTimeout))
	acknowledgementErr := acknowledgementDecoder.Decode(&acknowledgement)
	if acknowledgementErr != nil || acknowledgement.SessionID != request.SessionID || !acknowledgement.Persisted {
		cancel()
		exit := process.Wait()
		ended := launchEnded(request, exit)
		_ = encoder.Encode(Response{LaunchEnded: &ended})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	// Once persistence is acknowledged, a client half-close or disconnect is
	// treated as cancellation of this owned child, never as authority to touch
	// any unrelated process.
	remainingInput := bufio.NewReader(io.MultiReader(acknowledgementDecoder.Buffered(), requestReader))
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		monitorUnexpectedInputOrDisconnect(remainingInput, cancel)
	}()
	exit := process.Wait()
	ended := launchEnded(request, exit)
	_ = encoder.Encode(Response{LaunchEnded: &ended})
	_ = conn.Close()
	<-monitorDone
}

func monitorUnexpectedInputOrDisconnect(reader io.Reader, cancel context.CancelFunc) {
	buffered := bufio.NewReader(reader)
	for {
		value, err := buffered.ReadByte()
		if err != nil {
			cancel()
			return
		}
		switch value {
		case ' ', '\n', '\r', '\t':
			continue
		default:
			cancel()
			return
		}
	}
}

func (server *UDSServer) acquireLaunch() LaunchFailureCode {
	server.stateMu.Lock()
	defer server.stateMu.Unlock()
	if server.closed || server.ctx.Err() != nil {
		return LaunchFailureStartFailed
	}
	if server.launchActive {
		return LaunchFailureAlreadyRunning
	}
	server.launchActive = true
	return ""
}

func (server *UDSServer) releaseLaunch() {
	server.stateMu.Lock()
	server.launchActive = false
	server.stateMu.Unlock()
}

func launchEnded(request LaunchAsset, exit LaunchExit) LaunchEnded {
	if exit.Validate() != nil {
		exit = LaunchExit{Outcome: domain.SessionOutcomeInterrupted}
	}
	return LaunchEnded{
		SessionID: request.SessionID, WorkID: request.WorkID, EditionID: request.EditionID,
		AssetID: request.AssetID, EndedAt: time.Now().UTC(), Outcome: exit.Outcome, ExitCode: exit.ExitCode,
	}
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect Agent socket directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Agent socket parent is not a directory: %s", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("Agent socket directory must not be writable by group or other users: %s", path)
	}
	return nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing Agent socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket at Agent socket path %s", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("refusing to replace Agent socket owned by another user at %s", path)
	}
	conn, dialErr := net.DialTimeout("unix", path, 150*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("Lernae Agent socket is already active at %s", path)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
		return fmt.Errorf("cannot safely determine whether existing Agent socket is stale at %s: %w", path, dialErr)
	}
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("recheck existing Agent socket: %w", err)
	}
	if !os.SameFile(info, current) {
		return fmt.Errorf("Agent socket changed while checking stale path %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale Agent socket at %s: %w", path, err)
	}
	return nil
}
