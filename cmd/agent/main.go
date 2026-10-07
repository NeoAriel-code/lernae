package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"lernae/internal/agent"
	"lernae/internal/agentops"
	"lernae/internal/config"
	"lernae/internal/domain"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("Lernae Agent stopped with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	settings, err := config.LoadAgent()
	if err != nil {
		return err
	}
	server, cache, err := newAgentServer(settings)
	if err != nil {
		return err
	}
	defer cache.Close()
	defer server.Close()
	slog.Info("Lernae Agent started", "socket_path", settings.SocketPath)

	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve() }()
	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-signalContext.Done():
		slog.Info("Lernae Agent shutdown requested")
		return server.Close()
	case err := <-serveResult:
		if err != nil {
			return err
		}
		return nil
	}
}

func newAgentServer(settings config.Agent) (*agent.UDSServer, *agentops.Cache, error) {
	cache, err := agentops.OpenCacheWithStagingPath(settings.CachePath, settings.StagingPath)
	if err != nil {
		return nil, nil, err
	}
	executor, err := agentops.NewRcloneRestoreExecutor(cache)
	if err != nil {
		_ = cache.Close()
		return nil, nil, err
	}
	launchExecutor := &dolphinLaunchExecutor{cache: cache, runner: agentops.NewDolphinRunner()}
	server, err := agent.ListenUDSWithExecutors(settings.SocketPath, executor, launchExecutor)
	if err != nil {
		_ = cache.Close()
		return nil, nil, err
	}
	return server, cache, nil
}

type dolphinLaunchExecutor struct {
	cache  *agentops.Cache
	runner *agentops.DolphinRunner
}

func (executor *dolphinLaunchExecutor) Start(ctx context.Context, request agent.LaunchAsset) (agent.LaunchProcess, error) {
	if executor == nil || executor.cache == nil || executor.runner == nil || request.Validate() != nil {
		return nil, errors.New("invalid GameCube launch request")
	}
	work, edition, asset, parts := request.DomainRecords()
	image, err := executor.cache.OpenReadyGameCubeImageForSession(string(request.SessionID), work, edition, asset, parts)
	if err != nil {
		if errors.Is(err, agentops.ErrLocalReadyImageInvalid) {
			return nil, agent.ErrLaunchLocalCacheInvalid
		}
		return nil, err
	}
	defer func() { _ = image.Close() }()
	process, err := executor.runner.Start(ctx, image)
	if err != nil {
		if errors.Is(err, agentops.ErrLocalReadyImageInvalid) {
			return nil, agent.ErrLaunchLocalCacheInvalid
		}
		return nil, err
	}
	return dolphinLaunchProcess{process: process}, nil
}

type dolphinLaunchProcess struct {
	process *agentops.DolphinProcess
}

func (process dolphinLaunchProcess) Wait() agent.LaunchExit {
	err := process.process.Wait()
	if err == nil {
		return agent.LaunchExit{Outcome: domain.SessionOutcomeNormalExit, ExitCode: intPointer(0)}
	}
	var exitError *agentops.DolphinExitError
	if errors.As(err, &exitError) {
		return agent.LaunchExit{Outcome: domain.SessionOutcomeNonZeroExit, ExitCode: intPointer(exitError.ExitCode)}
	}
	return agent.LaunchExit{Outcome: domain.SessionOutcomeInterrupted}
}

func intPointer(value int) *int { return &value }
