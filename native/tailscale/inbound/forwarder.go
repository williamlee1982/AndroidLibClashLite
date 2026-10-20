package inbound

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// State is the lifecycle state reported for one configured forward.
type State string

const (
	StateListening State = "Listening"
	StateWaiting   State = "Waiting"
	StateError     State = "Error"
)

// Snapshot is the public, non-secret status of one forward.
type Snapshot struct {
	Name        string
	Network     string
	Listen      uint16
	Target      string
	State       State
	Error       string
	ActiveConns int
	TotalConns  uint64
	Rejected    uint64
}

// Options supplies the platform-specific listener and dialer operations.
type Options struct {
	Listen   func(network, addr string) (net.Listener, error)
	Dial     func(context.Context, string, string) (net.Conn, error)
	Ready    func() bool
	Wait     func(context.Context, time.Duration) error
	Logger   func(string, ...any)
	MaxConns int
}

type forwardRuntime struct {
	configured Forward
	options    Options
	mu         sync.Mutex
	state      State
	err        string
	active     int
	total      uint64
	rejected   uint64
	listener   net.Listener
	conns      map[net.Conn]struct{}
	stop       chan struct{}
	stopOnce   sync.Once
	done       chan struct{}
}

// Manager owns listeners and forwarded connections for one outbound.
type Manager struct {
	forwards []*forwardRuntime
	started  bool
	mu       sync.Mutex
}

func NewManager(forwards []Forward, options Options) *Manager {
	if options.MaxConns <= 0 {
		options.MaxConns = 64
	}
	if options.Ready == nil {
		options.Ready = func() bool { return true }
	}
	if options.Listen == nil {
		options.Listen = net.Listen
	}
	if options.Dial == nil {
		options.Dial = (&net.Dialer{}).DialContext
	}
	if options.Wait == nil {
		options.Wait = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if options.Logger == nil {
		options.Logger = func(format string, args ...any) { log.Printf(format, args...) }
	}
	m := &Manager{}
	for _, f := range forwards {
		m.forwards = append(m.forwards, &forwardRuntime{configured: f, options: options, state: StateWaiting, conns: make(map[net.Conn]struct{}), stop: make(chan struct{}), done: make(chan struct{})})
	}
	return m
}

func (m *Manager) Start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()
	for _, f := range m.forwards {
		go f.run()
	}
}

func (m *Manager) Stop() {
	for _, f := range m.forwards {
		f.stopOnce.Do(func() { close(f.stop) })
		// Closing resources here, rather than waiting for run to observe stop,
		// unblocks Accept and any active copies immediately. The run goroutine
		// then performs the final cleanup before signalling done.
		f.closeResources()
	}
	for _, f := range m.forwards {
		<-f.done
	}
}

func (m *Manager) Snapshot() []Snapshot {
	out := make([]Snapshot, 0, len(m.forwards))
	for _, f := range m.forwards {
		f.mu.Lock()
		out = append(out, Snapshot{Name: f.configured.Name, Network: f.configured.Network, Listen: f.configured.Listen, Target: f.configured.Target.String(), State: f.state, Error: f.err, ActiveConns: f.active, TotalConns: f.total, Rejected: f.rejected})
		f.mu.Unlock()
	}
	return out
}

func (f *forwardRuntime) run() {
	defer close(f.done)
	backoff := time.Second
	for {
		select {
		case <-f.stop:
			f.closeResources()
			return
		default:
		}
		if !f.options.Ready() {
			f.setState(StateWaiting, "")
			if f.wait(backoff) != nil {
				f.closeResources()
				return
			}
			continue
		}
		listener, err := f.options.Listen(f.configured.Network, ":"+portString(f.configured.Listen))
		if err != nil {
			f.setState(StateError, err.Error())
			if f.wait(backoff) != nil {
				f.closeResources()
				return
			}
			if backoff < time.Minute {
				backoff *= 2
				if backoff > time.Minute {
					backoff = time.Minute
				}
			}
			continue
		}
		backoff = time.Second
		f.mu.Lock()
		f.listener = listener
		f.state, f.err = StateListening, ""
		f.mu.Unlock()
		f.acceptLoop(listener)
		f.mu.Lock()
		f.listener = nil
		f.mu.Unlock()
		if !f.isStopping() {
			_ = listener.Close()
		}
	}
}

func (f *forwardRuntime) wait(d time.Duration) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-f.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return f.options.Wait(ctx, d)
}

func (f *forwardRuntime) acceptLoop(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if f.isStopping() || errors.Is(err, net.ErrClosed) {
				return
			}
			f.setState(StateError, err.Error())
			return
		}
		f.mu.Lock()
		if f.active >= f.options.MaxConns {
			f.rejected++
			f.mu.Unlock()
			_ = conn.Close()
			continue
		}
		f.active++
		f.total++
		f.conns[conn] = struct{}{}
		f.mu.Unlock()
		f.options.Logger("inbound %s %s -> %s", f.configured.Name, conn.RemoteAddr(), f.configured.Target)
		go f.handle(conn)
	}
}

func (f *forwardRuntime) handle(client net.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	target, err := f.options.Dial(ctx, f.configured.Network, f.configured.Target.String())
	cancel()
	if err != nil {
		f.setState(StateListening, err.Error())
		_ = client.Close()
		f.finish(client, true)
		return
	}
	f.mu.Lock()
	f.conns[target] = struct{}{}
	f.mu.Unlock()

	var wg sync.WaitGroup
	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		_, copyErr := io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok && copyErr == nil {
			_ = cw.CloseWrite()
		}
	}
	wg.Add(2)
	go copyHalf(target, client)
	go copyHalf(client, target)
	wg.Wait()
	_ = client.Close()
	_ = target.Close()
	f.finish(client, true)
	f.finish(target, false)
	f.options.Logger("inbound %s closed", f.configured.Name)
}

func (f *forwardRuntime) finish(conn net.Conn, accepted bool) {
	f.mu.Lock()
	if _, ok := f.conns[conn]; ok {
		delete(f.conns, conn)
		if accepted && f.active > 0 {
			f.active--
		}
	}
	f.mu.Unlock()
}

func (f *forwardRuntime) closeResources() {
	f.mu.Lock()
	listener := f.listener
	conns := make([]net.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

func (f *forwardRuntime) setState(state State, err string) {
	f.mu.Lock()
	f.state, f.err = state, err
	f.mu.Unlock()
}
func (f *forwardRuntime) isStopping() bool {
	select {
	case <-f.stop:
		return true
	default:
		return false
	}
}
func portString(v uint16) string {
	if v == 0 {
		return "0"
	}
	var b [5]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
