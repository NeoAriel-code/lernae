// Package playback coordinates a local Agent launch with durable Session
// lifecycle persistence. It is an internal Server service, not an HTTP route.
package playback

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"lernae/internal/agent"
	"lernae/internal/domain"
)

const sessionCleanupTimeout = 5 * time.Second

var (
	ErrServiceClosed                    = errors.New("playback service is closed")
	ErrTerminalUnconfirmed              = errors.New("Agent terminal event was not received; launch outcome remains unconfirmed")
	ErrSessionStartNotificationRejected = errors.New("Session start notification was not accepted")
)

// LaunchClient is implemented by the typed local Agent UDS client.
type LaunchClient interface {
	LaunchAsset(context.Context, agent.LaunchAsset, func(agent.LaunchStarted) bool) (agent.LaunchEnded, error)
}

// SessionStore is the durable Session lifecycle boundary used by launch.
type SessionStore interface {
	NewID() (domain.SessionID, error)
	StartWithID(context.Context, domain.SessionID, domain.WorkID, domain.EditionID, domain.AssetID, time.Time) (domain.Session, error)
	Finish(context.Context, domain.SessionID, domain.SessionOutcome, time.Time) error
}

// Service owns Agent launch calls under the Server lifetime. Callers do not
// supply a request context: an HTTP disconnect cannot cancel gameplay.
type Service struct {
	ctx      context.Context
	cancel   context.CancelFunc
	client   LaunchClient
	sessions SessionStore
	mu       sync.Mutex
	closed   bool
	closeErr error
	launches sync.WaitGroup
}

func NewService(parent context.Context, client LaunchClient, sessionStore SessionStore) (*Service, error) {
	if parent == nil || client == nil || sessionStore == nil {
		return nil, errors.New("playback service requires Server context, Agent client, and Session store")
	}
	ctx, cancel := context.WithCancel(parent)
	return &Service{ctx: ctx, cancel: cancel, client: client, sessions: sessionStore}, nil
}

// Launch starts the requested fixed-shape Agent operation and persists its
// Session only after receiving process-start confirmation.
func (service *Service) Launch(request agent.LaunchAsset) (agent.LaunchEnded, error) {
	return service.LaunchWithStarted(request, nil)
}

// LaunchWithStarted notifies the owning operation after the Session start row
// is durable and before acknowledging process start to the Agent. Returning
// false rejects that acknowledgement; the Agent then stops and reaps only the
// process it just started.
func (service *Service) LaunchWithStarted(request agent.LaunchAsset, notify func(domain.Session) bool) (agent.LaunchEnded, error) {
	service.mu.Lock()
	if service.closed {
		service.mu.Unlock()
		return agent.LaunchEnded{}, ErrServiceClosed
	}
	service.launches.Add(1)
	ctx := service.ctx
	service.mu.Unlock()
	defer service.launches.Done()

	id, err := service.sessions.NewID()
	if err != nil {
		return agent.LaunchEnded{}, fmt.Errorf("allocate Session correlation ID: %w", err)
	}
	request.SessionID = id
	if err := request.Validate(); err != nil {
		return agent.LaunchEnded{}, err
	}

	var started bool
	var startObserved bool
	var startErr error
	var startOnce sync.Once
	onStarted := func(event agent.LaunchStarted) bool {
		acknowledge := false
		startOnce.Do(func() {
			if event.ValidateFor(request) != nil {
				return
			}
			startObserved = true
			if ctx.Err() != nil {
				return
			}
			session, err := service.sessions.StartWithID(ctx, id, request.WorkID, request.EditionID, request.AssetID, event.StartedAt)
			startErr = err
			if startErr == nil {
				started = true
				if notify != nil && !notify(session) {
					startErr = ErrSessionStartNotificationRejected
					return
				}
				acknowledge = true
			}
		})
		return acknowledge
	}

	ended, launchErr := service.client.LaunchAsset(ctx, request, onStarted)
	terminalReceived := ended.ValidateFor(request) == nil
	stateUnconfirmed := errors.Is(launchErr, agent.ErrLaunchStateUnconfirmed)
	if (startObserved || stateUnconfirmed) && !terminalReceived {
		unresolved := errors.Join(ErrTerminalUnconfirmed, startErr, launchErr)
		service.recordCloseError(unresolved)
		return agent.LaunchEnded{
			SessionID: request.SessionID, WorkID: request.WorkID,
			EditionID: request.EditionID, AssetID: request.AssetID,
		}, unresolved
	}
	if started {
		// A valid terminal event proves the Agent completed Wait; a concurrent
		// caller cancellation must not rewrite that observed process outcome.
		if err := service.finishDetached(ctx, id, ended.Outcome, ended.EndedAt); err != nil {
			if launchErr != nil {
				return ended, errors.Join(launchErr, fmt.Errorf("finalize interrupted Session: %w", err))
			}
			return ended, fmt.Errorf("finalize Session: %w", err)
		}
	}
	if startErr != nil {
		return ended, errors.Join(fmt.Errorf("persist confirmed active Session: %w", startErr), launchErr)
	}
	if launchErr != nil {
		return ended, launchErr
	}
	return ended, nil
}

// Close cancels all active Agent interactions and waits until their child
// processes have been reaped and any confirmed Sessions finalized.
func (service *Service) Close() error {
	service.mu.Lock()
	if !service.closed {
		service.closed = true
		service.cancel()
	}
	service.mu.Unlock()
	service.launches.Wait()
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.closeErr
}

func (service *Service) recordCloseError(err error) {
	if err == nil {
		return
	}
	service.mu.Lock()
	service.closeErr = errors.Join(service.closeErr, err)
	service.mu.Unlock()
}

func (service *Service) finishDetached(parent context.Context, id domain.SessionID, outcome domain.SessionOutcome, endedAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), sessionCleanupTimeout)
	defer cancel()
	return service.sessions.Finish(ctx, id, outcome, endedAt)
}
