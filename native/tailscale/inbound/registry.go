package inbound

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
)

// ErrUnsupportedInProvider is reported for proxy-provider members that declare
// forwards: a provider refresh replaces its outbounds without ApplyConfig, so
// listeners would silently vanish (design D7).
const ErrUnsupportedInProvider = "unsupported in proxy providers"

// Controller is what one tailscale outbound exposes to the registry. The
// outbound overlay builds exactly one per outbound instance, so the pointer
// identifies the instance across reconciles.
type Controller struct {
	// StateDir is the resolved tsnet state directory, the node identity.
	StateDir string
	Forwards Forwards
	// Start begins the outbound's tsnet start. It must not block on login.
	Start func() error
	// Ready reports that the node is up so listeners can be registered.
	Ready  func() bool
	Listen func(network, addr string) (net.Listener, error)
	Dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	// Close closes the whole outbound. It may call Registry.Forget.
	Close  func() error
	Logger func(string, ...any)

	forgotten atomic.Bool
}

// Entry is one outbound of the running configuration. Provider is empty for
// top-level proxies.
type Entry struct {
	Name       string
	Provider   string
	TopLevel   bool
	Controller *Controller
}

// Provider is implemented by tailscale outbounds (and the proxy wrappers that
// forward to them) through the Mihomo overlay.
type Provider interface {
	InboundController() *Controller
}

type registration struct {
	entry    Entry
	manager  *Manager
	startErr string
}

// Registry tracks the outbounds of the running configuration and owns the
// forward managers of the top-level ones that declare forwards.
type Registry struct {
	reconcileMu sync.Mutex // serializes Reconcile; never held by Forget
	mu          sync.Mutex // guards running
	running     map[*Controller]*registration
}

func NewRegistry() *Registry {
	return &Registry{running: map[*Controller]*registration{}}
}

// Default is the process-wide registry used by the Mihomo overlay.
var Default = NewRegistry()

// Reconcile makes the registry match the running configuration:
//  1. outbounds no longer running get their listeners stopped;
//  2. a superseded outbound whose state dir equals a running one's is closed,
//     so one tsnet identity never runs twice (design D4), before
//  3. new top-level outbounds with forwards are started eagerly (D5) and
//     their forwards begin listening once the node is ready.
//
// Provider members are recorded but never started (D7). Start and Close run
// without the registry lock because Close re-enters through Forget.
func (r *Registry) Reconcile(entries []Entry) {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()

	next := make(map[*Controller]*registration, len(entries))
	byController := make(map[*Controller]Entry, len(entries))
	order := make([]*Controller, 0, len(entries))
	dirs := map[string]struct{}{}
	for _, entry := range entries {
		c := entry.Controller
		if c == nil {
			continue
		}
		if _, dup := next[c]; dup {
			continue // first (top-level) occurrence wins
		}
		next[c] = nil
		byController[c] = entry
		order = append(order, c)
		if c.StateDir != "" {
			dirs[c.StateDir] = struct{}{}
		}
	}

	var stop []*Manager
	var closeOld []*Controller
	var start []*Controller
	r.mu.Lock()
	for c, reg := range r.running {
		if _, still := next[c]; still {
			next[c] = reg
			continue
		}
		delete(r.running, c)
		if reg.manager != nil {
			stop = append(stop, reg.manager)
		}
		if _, superseded := dirs[c.StateDir]; superseded && c.StateDir != "" && !c.forgotten.Load() {
			closeOld = append(closeOld, c)
		}
	}
	for _, c := range order {
		if next[c] != nil || c.forgotten.Load() {
			continue
		}
		reg := &registration{entry: byController[c]}
		r.running[c] = reg
		if reg.entry.TopLevel && reg.entry.Provider == "" && c.Forwards.HasForwards() {
			start = append(start, c)
		}
	}
	r.mu.Unlock()

	for _, m := range stop {
		m.Stop()
	}
	for _, c := range closeOld {
		if c.Close != nil {
			if err := c.Close(); err != nil {
				c.log("inbound: close superseded outbound: %v", err)
			}
		}
	}
	for _, c := range start {
		r.start(c)
	}
}

func (r *Registry) start(c *Controller) {
	var startErr string
	if c.Start != nil {
		if err := c.Start(); err != nil {
			startErr = "start: " + err.Error()
		}
	}
	var manager *Manager
	if startErr == "" {
		manager = NewManager(c.Forwards, Options{
			Listen:   c.Listen,
			Dial:     c.Dial,
			Ready:    c.Ready,
			Logger:   c.Logger,
			MaxConns: DefaultMaxConns,
		})
	}
	r.mu.Lock()
	reg := r.running[c]
	if reg == nil || c.forgotten.Load() {
		r.mu.Unlock()
		return // closed or replaced while starting
	}
	reg.startErr, reg.manager = startErr, manager
	if manager != nil {
		manager.Start() // under r.mu so Forget cannot miss the manager
	}
	r.mu.Unlock()
}

// Forget stops the listeners of a closed outbound. It is idempotent, never
// closes the outbound itself and keeps a later Reconcile from restarting it.
func (r *Registry) Forget(c *Controller) {
	if c == nil {
		return
	}
	c.forgotten.Store(true)
	r.mu.Lock()
	reg := r.running[c]
	delete(r.running, c)
	r.mu.Unlock()
	if reg != nil && reg.manager != nil {
		reg.manager.Stop()
	}
}

// Snapshot reports the forwards of one outbound, or nil when it has none or
// is not part of the running configuration.
func (r *Registry) Snapshot(c *Controller) []Snapshot {
	if c == nil {
		return nil
	}
	r.mu.Lock()
	reg := r.running[c]
	var manager *Manager
	var state State
	var message string
	if reg != nil {
		manager = reg.manager
		switch {
		case !reg.entry.TopLevel || reg.entry.Provider != "":
			state, message = StateError, ErrUnsupportedInProvider
		case reg.startErr != "":
			state, message = StateError, reg.startErr
		default:
			state = StateWaiting
		}
	}
	r.mu.Unlock()
	if reg == nil || !c.Forwards.HasForwards() {
		return nil
	}
	if manager != nil {
		return manager.Snapshot()
	}
	out := make([]Snapshot, 0, len(c.Forwards))
	for _, f := range c.Forwards {
		out = append(out, Snapshot{Name: f.Name, Network: f.Network, Listen: f.Listen,
			Target: f.Target.String(), State: state, Error: message})
	}
	return out
}

func (c *Controller) log(format string, args ...any) {
	if c.Logger != nil {
		c.Logger(format, args...)
	}
}
