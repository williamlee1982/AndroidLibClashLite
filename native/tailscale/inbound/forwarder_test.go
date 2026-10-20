package inbound

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type testPipeListener struct {
	addr   net.Addr
	accept chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newTestPipeListener() *testPipeListener {
	return &testPipeListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 20022}, accept: make(chan net.Conn), closed: make(chan struct{})}
}
func (l *testPipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.accept:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *testPipeListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *testPipeListener) Addr() net.Addr { return l.addr }
func (l *testPipeListener) connect() net.Conn {
	client, server := net.Pipe()
	select {
	case l.accept <- server:
		return client
	case <-l.closed:
		_ = client.Close()
		_ = server.Close()
		return nil
	}
}

func testForward(listen uint16, targetPort uint16) Forward {
	return Forward{Name: "test", Network: "tcp", Listen: listen, Target: netip.MustParseAddrPort("127.0.0.1:" + itoa(targetPort))}
}
func itoa(v uint16) string { return fmt.Sprintf("%d", v) }

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestForwardCopiesBothDirections(t *testing.T) {
	target, peer := net.Pipe()
	defer peer.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	m := NewManager([]Forward{testForward(20022, 20023)}, Options{
		Listen: func(string, string) (net.Listener, error) { return listener, nil },
		Dial:   func(context.Context, string, string) (net.Conn, error) { return target, nil },
		Ready:  func() bool { return true },
	})
	m.Start()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	payload := bytes.Repeat([]byte("x"), 1<<20)
	go func() { _, _ = peer.Write(payload) }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, got) {
		t.Fatal("target to client payload mismatch")
	}
	go func() { _, _ = client.Write(payload) }()
	got = make([]byte, len(payload))
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, got) {
		t.Fatal("client to target payload mismatch")
	}
	m.Stop()
}

func TestForwardHalfClose(t *testing.T) {
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetListener.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	m := NewManager([]Forward{testForward(20022, 20023)}, Options{
		Listen: func(string, string) (net.Listener, error) { return listener, nil },
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, targetListener.Addr().String())
		},
		Ready: func() bool { return true },
	})
	m.Start()
	targetDone := make(chan struct{})
	go func() {
		target, acceptErr := targetListener.Accept()
		if acceptErr != nil {
			return
		}
		defer target.Close()
		body, readErr := io.ReadAll(target)
		if readErr == nil && string(body) == "request" {
			_, _ = target.Write([]byte("reply"))
		}
		close(targetDone)
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = client.Write([]byte("request"))
	if tcp, ok := client.(interface{ CloseWrite() error }); ok {
		_ = tcp.CloseWrite()
	} else {
		t.Fatal("inbound test connection lacks half-close")
	}
	reply := make([]byte, 5)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatal(err)
	}
	if string(reply) != "reply" {
		t.Fatalf("client got %q", reply)
	}
	select {
	case <-targetDone:
	case <-time.After(time.Second):
		t.Fatal("target did not observe EOF")
	}
	m.Stop()
}

func TestForwardTargetDown(t *testing.T) {
	listener := newTestPipeListener()
	m := NewManager([]Forward{testForward(20022, 20023)}, Options{
		Listen: func(string, string) (net.Listener, error) { return listener, nil },
		Dial:   func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") },
		Ready:  func() bool { return true },
	})
	m.Start()
	client := listener.connect()
	waitFor(t, func() bool { return m.Snapshot()[0].TotalConns == 1 })
	if _, err := client.Write([]byte("x")); err == nil {
		t.Fatal("client remained open after refused target")
	}
	if got := m.Snapshot()[0]; got.State != StateListening || got.Error == "" {
		t.Fatalf("snapshot=%+v", got)
	}
	m.Stop()
}

func TestForwardConnLimit(t *testing.T) {
	listener := newTestPipeListener()
	var dials []net.Conn
	var mu sync.Mutex
	m := NewManager([]Forward{testForward(20022, 20023)}, Options{
		MaxConns: 2,
		Listen:   func(string, string) (net.Listener, error) { return listener, nil },
		Dial: func(context.Context, string, string) (net.Conn, error) {
			local, remote := net.Pipe()
			mu.Lock()
			dials = append(dials, remote)
			mu.Unlock()
			return local, nil
		},
		Ready: func() bool { return true },
	})
	m.Start()
	c1, c2, c3 := listener.connect(), listener.connect(), listener.connect()
	defer c1.Close()
	defer c2.Close()
	defer c3.Close()
	waitFor(t, func() bool { return m.Snapshot()[0].ActiveConns == 2 })
	if _, err := c3.Write([]byte("x")); err == nil {
		t.Fatal("third connection was not rejected")
	}
	if got := m.Snapshot()[0]; got.Rejected != 1 {
		t.Fatalf("snapshot=%+v", got)
	}
	m.Stop()
	mu.Lock()
	for _, c := range dials {
		_ = c.Close()
	}
	mu.Unlock()
}

func TestForwardStopClosesActive(t *testing.T) {
	listener := newTestPipeListener()
	m := NewManager([]Forward{testForward(20022, 20023)}, Options{
		Listen: func(string, string) (net.Listener, error) { return listener, nil },
		Dial:   func(context.Context, string, string) (net.Conn, error) { c, _ := net.Pipe(); return c, nil },
		Ready:  func() bool { return true },
	})
	m.Start()
	client := listener.connect()
	waitFor(t, func() bool { return m.Snapshot()[0].ActiveConns == 1 })
	m.Stop()
	if _, err := client.Write([]byte("x")); err == nil {
		t.Fatal("active client was not closed")
	}
	if rejected := listener.connect(); rejected != nil {
		_ = rejected.Close()
		t.Fatal("listener accepted after stop")
	}
}

func TestForwardListenRetry(t *testing.T) {
	listener := newTestPipeListener()
	attempts := 0
	waits := make([]time.Duration, 0, 2)
	m := NewManager([]Forward{testForward(20022, 20023)}, Options{
		Listen: func(string, string) (net.Listener, error) {
			attempts++
			if attempts < 3 {
				return nil, errors.New("busy")
			}
			return listener, nil
		},
		Wait:  func(context.Context, time.Duration) error { waits = append(waits, time.Second); return nil },
		Ready: func() bool { return true },
	})
	m.Start()
	waitFor(t, func() bool { return m.Snapshot()[0].State == StateListening })
	if attempts != 3 || len(waits) != 2 {
		t.Fatalf("attempts=%d waits=%v", attempts, waits)
	}
	m.Stop()
}

func TestForwardSnapshot(t *testing.T) {
	listener := newTestPipeListener()
	m := NewManager([]Forward{testForward(20022, 20023)}, Options{Listen: func(string, string) (net.Listener, error) { return listener, nil }, Ready: func() bool { return true }})
	m.Start()
	defer m.Stop()
	waitFor(t, func() bool { return m.Snapshot()[0].State == StateListening })
	s := m.Snapshot()[0]
	if s.Name != "test" || s.Network != "tcp" || s.Listen != 20022 || s.Target != "127.0.0.1:20023" || s.ActiveConns != 0 || s.TotalConns != 0 {
		t.Fatalf("snapshot=%+v", s)
	}
}

func TestForwardWaitingState(t *testing.T) {
	listener := newTestPipeListener()
	m := NewManager([]Forward{testForward(20022, 20023)}, Options{Listen: func(string, string) (net.Listener, error) { return listener, nil }, Ready: func() bool { return false }})
	m.Start()
	defer m.Stop()
	waitFor(t, func() bool { return m.Snapshot()[0].State == StateWaiting })
}
