package tailscale

import (
	"errors"
	"sync"
)

// Phase is the outbound-side lifecycle of a lazily started tsnet server.
type Phase int

const (
	PhaseIdle Phase = iota
	PhaseStarting
	PhaseServerStarted
	PhaseFailed
	PhaseClosed
)

// ErrClosed is returned for operations on a closed outbound.
var ErrClosed = errors.New("tailscale outbound closed")

// ErrNotStarted is returned when an operation needs a running tsnet server.
var ErrNotStarted = errors.New("tailscale outbound has not started")

// Lifecycle records start progress without touching the tsnet server, so
// status queries never start tsnet or race its Start/Close contract.
type Lifecycle struct {
	mu            sync.Mutex
	phase         Phase
	serverStarted bool
	err           error
}

type LifecycleSnapshot struct {
	Phase         Phase
	ServerStarted bool
	Err           error
}

func (l *Lifecycle) Starting() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.phase == PhaseIdle {
		l.phase = PhaseStarting
	}
}

func (l *Lifecycle) ServerStarted() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.phase != PhaseClosed {
		l.serverStarted = true
	}
}

// Finished records the result of the start attempt. It is idempotent.
func (l *Lifecycle) Finished(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.phase == PhaseClosed || l.phase == PhaseIdle {
		return
	}
	if err != nil {
		l.phase = PhaseFailed
		l.err = err
		return
	}
	l.phase = PhaseServerStarted
}

func (l *Lifecycle) Closed() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.phase = PhaseClosed
}

func (l *Lifecycle) Snapshot() LifecycleSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return LifecycleSnapshot{Phase: l.phase, ServerStarted: l.serverStarted, Err: l.err}
}
