package inbound

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
)

func registryForward() Forward {
	return Forward{Name: "ssh", Network: "tcp", Listen: 20022, Target: mustAddrPort("127.0.0.1:20022")}
}

func mustAddrPort(raw string) (out netip.AddrPort) {
	out, err := netip.ParseAddrPort(raw)
	if err != nil {
		panic(err)
	}
	return out
}

// events records Start/Close calls in order; the registry may call the hooks
// from the reconciling goroutine only, but the forwarders read Ready/Listen
// concurrently, so keep it locked.
type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	e.log = append(e.log, s)
	e.mu.Unlock()
}

func (e *events) count(s string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, v := range e.log {
		if v == s {
			n++
		}
	}
	return n
}

func (e *events) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.log...)
}

func fakeController(id, stateDir string, forwards []Forward, ev *events) *Controller {
	return &Controller{
		StateDir: stateDir,
		Forwards: forwards,
		Start: func() error {
			ev.add("start " + id)
			return nil
		},
		Ready:  func() bool { return true },
		Listen: func(string, string) (net.Listener, error) { return newTestPipeListener(), nil },
		Dial:   func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("unused") },
		Close: func() error {
			ev.add("close " + id)
			return nil
		},
	}
}

func TestReconcileClosesSupersededSameStateDir(t *testing.T) {
	ev := &events{}
	old := fakeController("old", "/state/shared", nil, ev) // previously lazy
	newOne := fakeController("new", "/state/shared", []Forward{registryForward()}, ev)
	different := fakeController("different", "/state/other", []Forward{registryForward()}, ev)
	r := NewRegistry()
	r.Reconcile([]Entry{{Name: "old", TopLevel: true, Controller: old}})
	if got := ev.snapshot(); len(got) != 0 {
		t.Fatalf("lazy outbound without forwards was touched: %v", got)
	}
	r.Reconcile([]Entry{
		{Name: "new", TopLevel: true, Controller: newOne},
		{Name: "different", TopLevel: true, Controller: different},
	})
	defer r.Reconcile(nil)
	if ev.count("close old") != 1 {
		t.Fatalf("superseded lazy outbound close count = %d", ev.count("close old"))
	}
	if ev.count("start new") != 1 || ev.count("start different") != 1 {
		t.Fatalf("forwarding outbounds not started once each: %v", ev.snapshot())
	}
	if ev.count("close new") != 0 || ev.count("close different") != 0 || ev.count("start old") != 0 {
		t.Fatalf("unexpected events: %v", ev.snapshot())
	}
	// D4: the old identity must be gone before the replacement starts.
	log := ev.snapshot()
	closeAt, startAt := -1, -1
	for i, v := range log {
		switch v {
		case "close old":
			closeAt = i
		case "start new":
			startAt = i
		}
	}
	if closeAt > startAt {
		t.Fatalf("replacement started before the superseded outbound closed: %v", log)
	}
	waitFor(t, func() bool {
		got := r.Snapshot(newOne)
		return len(got) == 1 && got[0].State == StateListening
	})
}

func TestReconcileKeepsUnchangedController(t *testing.T) {
	ev := &events{}
	c := fakeController("ts", "/state/same", []Forward{registryForward()}, ev)
	r := NewRegistry()
	r.Reconcile([]Entry{{Name: "ts", TopLevel: true, Controller: c}})
	r.Reconcile([]Entry{{Name: "ts", TopLevel: true, Controller: c}})
	defer r.Reconcile(nil)
	if ev.count("start ts") != 1 || ev.count("close ts") != 0 {
		t.Fatalf("unchanged controller restarted or closed: %v", ev.snapshot())
	}
}

func TestReconcileProviderMemberNotStarted(t *testing.T) {
	ev := &events{}
	provider := fakeController("provider", "/state/provider", []Forward{registryForward()}, ev)
	r := NewRegistry()
	r.Reconcile([]Entry{{Name: "provider-ts", Provider: "subscription", Controller: provider}})
	if got := ev.snapshot(); len(got) != 0 {
		t.Fatalf("provider member touched: %v", got)
	}
	got := r.Snapshot(provider)
	if len(got) != 1 || got[0].State != StateError || got[0].Error != "unsupported in proxy providers" ||
		got[0].Listen != 20022 || got[0].Target != "127.0.0.1:20022" {
		t.Fatalf("provider snapshot = %+v", got)
	}
}

func TestReconcileStopsRemovedForward(t *testing.T) {
	ev := &events{}
	controller := fakeController("ts", "/state/removed", []Forward{registryForward()}, ev)
	r := NewRegistry()
	r.Reconcile([]Entry{{Name: "ts", TopLevel: true, Controller: controller}})
	waitFor(t, func() bool { s := r.Snapshot(controller); return len(s) == 1 && s[0].State == StateListening })
	r.Reconcile(nil)
	if len(r.Snapshot(controller)) != 0 {
		t.Fatalf("removed controller still has snapshot: %+v", r.Snapshot(controller))
	}
	// Different state dir from everything running: left to Mihomo's GC.
	if ev.count("close ts") != 0 {
		t.Fatalf("removed outbound with a unique state dir was closed: %v", ev.snapshot())
	}
}

// Close re-enters the registry through Tailscale.Close -> tailscaleClosed ->
// Forget; Reconcile must not hold its lock while calling it.
func TestReconcileCloseMayReenterRegistry(t *testing.T) {
	ev := &events{}
	r := NewRegistry()
	old := fakeController("old", "/state/shared", []Forward{registryForward()}, ev)
	old.Close = func() error {
		ev.add("close old")
		r.Forget(old)
		return nil
	}
	newOne := fakeController("new", "/state/shared", []Forward{registryForward()}, ev)
	r.Reconcile([]Entry{{Name: "ts", TopLevel: true, Controller: old}})
	done := make(chan struct{})
	go func() {
		r.Reconcile([]Entry{{Name: "ts", TopLevel: true, Controller: newOne}})
		close(done)
	}()
	waitFor(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	defer r.Reconcile(nil)
	if ev.count("close old") != 1 {
		t.Fatalf("old not closed once: %v", ev.snapshot())
	}
}

func TestForgetStopsListenersWithoutClosing(t *testing.T) {
	ev := &events{}
	c := fakeController("ts", "/state/forget", []Forward{registryForward()}, ev)
	r := NewRegistry()
	r.Reconcile([]Entry{{Name: "ts", TopLevel: true, Controller: c}})
	waitFor(t, func() bool { s := r.Snapshot(c); return len(s) == 1 && s[0].State == StateListening })
	r.Forget(c)
	r.Forget(c) // idempotent
	if len(r.Snapshot(c)) != 0 || ev.count("close ts") != 0 {
		t.Fatalf("forget: snapshot=%+v events=%v", r.Snapshot(c), ev.snapshot())
	}
	// A forgotten (closed) controller must not be restarted by a later reconcile
	// that still lists it, e.g. a GC-closed adapter racing a reload.
	r.Reconcile([]Entry{{Name: "ts", TopLevel: true, Controller: c}})
	if ev.count("start ts") != 1 {
		t.Fatalf("forgotten controller restarted: %v", ev.snapshot())
	}
}

func TestReconcileStartErrorIsReported(t *testing.T) {
	ev := &events{}
	c := fakeController("ts", "/state/err", []Forward{registryForward()}, ev)
	c.Start = func() error { return errors.New("boom") }
	c.Ready = func() bool { return false }
	r := NewRegistry()
	r.Reconcile([]Entry{{Name: "ts", TopLevel: true, Controller: c}})
	defer r.Reconcile(nil)
	got := r.Snapshot(c)
	if len(got) != 1 || got[0].State != StateError || got[0].Error != "start: boom" {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestSnapshotNilAndUnknown(t *testing.T) {
	r := NewRegistry()
	if r.Snapshot(nil) != nil || r.Snapshot(&Controller{}) != nil {
		t.Fatal("unknown controller has a snapshot")
	}
}
