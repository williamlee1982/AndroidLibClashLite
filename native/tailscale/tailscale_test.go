package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metacubex/http"
	"github.com/metacubex/http/httptest"
)

// Fixture values only; they are not real credentials.
const (
	fixtureLoginURL = "https://login.example.test/a/0123456789abcdef"
	fixtureAuthKey  = "tskey-auth-kFIXTURE0CNTRL-FIXTUREFIXTUREFIXTURE"
)

func TestFromSnapshotMapsStatesAndHidesLoginURL(t *testing.T) {
	cases := map[string]State{
		"NoState": StateStarting, "Starting": StateStarting, "NeedsLogin": StateNeedsLogin,
		"NeedsMachineAuth": StateNeedsMachineAuth, "Running": StateRunning,
		"Stopped": StateStopped, "InUseOtherUser": StateError, "Bogus": StateError,
	}
	for backend, want := range cases {
		got := FromSnapshot("ts", Snapshot{BackendState: backend, AuthURL: fixtureLoginURL})
		if got.State != want {
			t.Errorf("%s: state %s, want %s", backend, got.State, want)
		}
		if got.LoginURLAvailable != (want == StateNeedsLogin) {
			t.Errorf("%s: loginUrlAvailable %v", backend, got.LoginURLAvailable)
		}
		data, _ := json.Marshal(got)
		if strings.Contains(string(data), "/a/") || strings.Contains(string(data), "login.example") {
			t.Errorf("%s: status JSON leaks the login URL: %s", backend, data)
		}
	}
}

func peerIPsJSON(t *testing.T, status Status) (present bool, ips []string) {
	t.Helper()
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	field, ok := raw["peerIps"]
	if !ok {
		return false, nil
	}
	if string(field) == "null" {
		t.Fatalf("peerIps is null; omit the field or send an array: %s", data)
	}
	if err := json.Unmarshal(field, &ips); err != nil {
		t.Fatalf("peerIps %s: %v", field, err)
	}
	for _, ip := range ips {
		parsed, err := netip.ParseAddr(ip)
		if err != nil || parsed.String() != ip || parsed.Zone() != "" {
			t.Fatalf("peerIps element %q is not a canonical address without a port, zone, or name", ip)
		}
	}
	return true, ips
}

func TestFromSnapshotOmitsPeerIPsWhenPeersWereNotQueried(t *testing.T) {
	got := FromSnapshot("ts", Snapshot{
		BackendState: "Running",
		TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.2")},
		// A stale candidate must not be reported when the backend did not
		// query the netmap. The app treats a missing field as "unknown".
		PeerIPs: []netip.Addr{netip.MustParseAddr("100.64.0.9")},
	})
	if present, ips := peerIPsJSON(t, got); present {
		t.Fatalf("peerIps = %v, want the field omitted", ips)
	}
}

func TestFromSnapshotEmitsEmptyPeerIPsWhenTheNetmapHasNoOtherNodes(t *testing.T) {
	got := FromSnapshot("ts", Snapshot{
		BackendState: "Running",
		TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.2")},
		PeersQueried: true,
	})
	present, ips := peerIPsJSON(t, got)
	if !present || len(ips) != 0 {
		t.Fatalf("peerIps present=%v ips=%v, want []", present, ips)
	}
	data, _ := json.Marshal(got)
	if !strings.Contains(string(data), `"peerIps":[]`) {
		t.Fatalf("empty report must be an array, not an omitted field: %s", data)
	}
}

func TestFromSnapshotEmptyPeerReportWhenNothingAssignableRemains(t *testing.T) {
	got := FromSnapshot("ts", Snapshot{
		BackendState: "NeedsLogin",
		AuthURL:      fixtureLoginURL,
		PeersQueried: true,
		PeerIPs:      []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("100.115.92.10")},
	})
	if got.State != StateNeedsLogin || !got.LoginURLAvailable {
		t.Fatalf("state %+v", got)
	}
	present, ips := peerIPsJSON(t, got)
	if !present || len(ips) != 0 {
		t.Fatalf("filtered report present=%v ips=%v, want []", present, ips)
	}
}

func TestFromSnapshotReportsOtherNodesTailscaleIPsOnly(t *testing.T) {
	self4 := netip.MustParseAddr("100.64.0.2")
	self6 := netip.MustParseAddr("fd7a:115c:a1e0::1")
	got := FromSnapshot("ts", Snapshot{
		BackendState:     "Running",
		AuthURL:          fixtureLoginURL,
		TailscaleIPs:     []netip.Addr{self6, self4},
		SelfTailscaleIPs: []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::8")},
		PeersQueried:     true,
		PeerIPs: []netip.Addr{
			netip.MustParseAddr("fd7a:115c:a1e0::5"),
			self4,
			netip.MustParseAddr("100.64.0.9"),
			netip.MustParseAddr("100.64.0.9"),
			netip.MustParseAddr("8.8.8.8"),
			netip.MustParseAddr("100.115.92.10"),
			netip.MustParseAddr("fd00::1"),
			{},
			netip.MustParseAddr("::ffff:100.64.0.3"),
			netip.MustParseAddr("fd7a:115c:a1e0:0:0:0:0:5"),
			netip.MustParseAddr("fd7a:115c:a1e0::8"),
			netip.MustParseAddr("fd7a:115c:a1e0::5%eth0"),
		},
		Health: []string{"warning with " + fixtureAuthKey},
	})
	present, ips := peerIPsJSON(t, got)
	if !present {
		t.Fatal("queried peers omitted peerIps")
	}
	want := []string{"100.64.0.3", "100.64.0.9", "fd7a:115c:a1e0::5"}
	if strings.Join(ips, ",") != strings.Join(want, ",") {
		t.Fatalf("peerIps %v, want %v", ips, want)
	}
	if strings.Join(got.TailnetIPs, ",") != "100.64.0.2,fd7a:115c:a1e0::1" {
		t.Fatalf("self addresses changed: %v", got.TailnetIPs)
	}
	data, _ := json.Marshal(got)
	for _, secret := range []string{"0123456789abcdef", "kFIXTURE", "8.8.8.8", "100.115.92.10", "fd00::1", "%eth0", "phone", "nodekey"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("%q leaked into status: %s", secret, data)
		}
	}
}

func TestFromSnapshotOrdersAddressesAndTrimsNames(t *testing.T) {
	got := FromSnapshot("ts", Snapshot{
		BackendState:   "Running",
		TailscaleIPs:   []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::1"), {}, netip.MustParseAddr("100.64.0.2")},
		HostName:       " phone ",
		DNSName:        "phone.example-tailnet.ts.net.",
		MagicDNSSuffix: "example-tailnet.ts.net.",
		Health:         []string{"", "warning with " + fixtureAuthKey},
	})
	if strings.Join(got.TailnetIPs, ",") != "100.64.0.2,fd7a:115c:a1e0::1" {
		t.Fatalf("ips %v", got.TailnetIPs)
	}
	if got.Hostname != "phone" || got.DNSName != "phone.example-tailnet.ts.net" || got.MagicDNSSuffix != "example-tailnet.ts.net" {
		t.Fatalf("names %+v", got)
	}
	if len(got.Health) != 1 || strings.Contains(got.Health[0], "kFIXTURE") {
		t.Fatalf("health %v", got.Health)
	}
}

func TestFromSnapshotRejectsNonHTTPSLoginURL(t *testing.T) {
	for _, raw := range []string{"http://login.example.test/a/x", "javascript:alert(1)", "https:///a/x", "https://user@login.example.test/a/x"} {
		if FromSnapshot("ts", Snapshot{BackendState: "NeedsLogin", AuthURL: raw}).LoginURLAvailable {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestRedact(t *testing.T) {
	input := strings.Join([]string{
		"To authenticate, visit: " + fixtureLoginURL,
		"key " + fixtureAuthKey,
		"nodekey:0123456789abcdef0123456789abcdef",
		"Authorization: Bearer abcdefghijklmnop",
		"Cookie: session=secret-value",
		"auth-key: plain-secret",
	}, "\n")
	out := Redact(input)
	for _, secret := range []string{"0123456789abcdef", "kFIXTURE", "abcdefghijklmnop", "secret-value", "plain-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q survived redaction:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "https://login.example.test/a/[redacted]") {
		t.Errorf("login URL host should stay for diagnostics:\n%s", out)
	}
	if line := RedactLog("visit " + fixtureLoginURL); !strings.Contains(line, "open the login flow from the app") {
		t.Errorf("RedactLog hint missing: %s", line)
	}
}

func TestLifecycle(t *testing.T) {
	var l Lifecycle
	if l.Snapshot().Phase != PhaseIdle {
		t.Fatal("not idle")
	}
	l.Finished(nil) // ignored before Starting
	if l.Snapshot().Phase != PhaseIdle {
		t.Fatal("Finished before Starting changed phase")
	}
	l.Starting()
	l.ServerStarted()
	l.Finished(errors.New("prefs"))
	if s := l.Snapshot(); s.Phase != PhaseFailed || !s.ServerStarted || s.Err == nil {
		t.Fatalf("%+v", s)
	}
	l.Closed()
	l.Starting()
	l.ServerStarted()
	l.Finished(nil)
	if s := l.Snapshot(); s.Phase != PhaseClosed {
		t.Fatalf("closed lifecycle reopened: %+v", s)
	}
}

type fakeBackend struct {
	mu       sync.Mutex
	life     *Lifecycle
	startErr error
	loginErr error
	snapshot Snapshot
	queryErr error
	logins   int
	logouts  int
	starts   int
}

func (f *fakeBackend) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.life.Starting()
	if f.startErr == nil {
		f.life.ServerStarted()
	}
	f.life.Finished(f.startErr)
	return f.startErr
}

func (f *fakeBackend) Query(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot, f.queryErr
}

func (f *fakeBackend) LoginInteractive(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logins++
	if f.loginErr != nil {
		return f.loginErr
	}
	f.snapshot.AuthURL = fixtureLoginURL
	return nil
}

func (f *fakeBackend) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts++
	f.snapshot = Snapshot{BackendState: "NeedsLogin"}
	return nil
}

func newController() (*Controller, *fakeBackend) {
	life := &Lifecycle{}
	backend := &fakeBackend{life: life, snapshot: Snapshot{BackendState: "NeedsLogin"}}
	return &Controller{Name: "ts", Lifecycle: life, Backend: backend, LoginTimeout: time.Second}, backend
}

func TestControllerStatusNeverStarts(t *testing.T) {
	c, backend := newController()
	if got := c.Status(context.Background()); got.State != StateIdle {
		t.Fatalf("state %s", got.State)
	}
	if backend.starts != 0 {
		t.Fatal("Status started tsnet")
	}
	if _, err := c.LoginURL(context.Background()); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("LoginURL before start: %v", err)
	}
	if err := c.Logout(context.Background()); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Logout before start: %v", err)
	}
}

func TestControllerLoginFlow(t *testing.T) {
	c, backend := newController()
	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !c.Status(context.Background()).LoginURLAvailable {
		if time.Now().After(deadline) {
			t.Fatal("login URL never became available")
		}
		time.Sleep(10 * time.Millisecond)
	}
	url, err := c.LoginURL(context.Background())
	if err != nil || url != fixtureLoginURL {
		t.Fatalf("LoginURL %q %v", url, err)
	}
	if backend.logins != 1 {
		t.Fatalf("logins %d", backend.logins)
	}
	if err := c.Logout(context.Background()); err != nil || backend.logouts != 1 {
		t.Fatalf("logout %v %d", err, backend.logouts)
	}
	if _, err := c.LoginURL(context.Background()); !errors.Is(err, ErrLoginURLUnavailable) {
		t.Fatalf("after logout: %v", err)
	}
}

func TestControllerOmitsPeerIPsUntilTheyAreQueried(t *testing.T) {
	idle, _ := newController()
	if present, ips := peerIPsJSON(t, idle.Status(context.Background())); present {
		t.Fatalf("idle peerIps = %v", ips)
	}

	running, backend := newController()
	backend.snapshot = Snapshot{
		BackendState: "Running",
		TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.2")},
		PeersQueried: true,
		PeerIPs:      []netip.Addr{netip.MustParseAddr("100.64.0.9"), netip.MustParseAddr("100.64.0.2")},
	}
	_ = backend.Start()
	present, ips := peerIPsJSON(t, running.Status(context.Background()))
	if !present || strings.Join(ips, ",") != "100.64.0.9" {
		t.Fatalf("running peerIps present=%v ips=%v", present, ips)
	}

	backend.mu.Lock()
	backend.snapshot.BackendState = "Stopped"
	backend.snapshot.PeerIPs = []netip.Addr{netip.MustParseAddr("100.64.0.9")}
	backend.mu.Unlock()
	running.Lifecycle.LoginResult(running.Lifecycle.LoginStarted(), errors.New("login failed"))
	failed := running.Status(context.Background())
	if failed.State != StateError {
		t.Fatalf("state %s", failed.State)
	}
	if present, ips := peerIPsJSON(t, failed); present {
		t.Fatalf("error status must not report peers: %v", ips)
	}

	backend.mu.Lock()
	backend.queryErr = errors.New("local api down")
	backend.snapshot = Snapshot{BackendState: "Running", PeersQueried: true, PeerIPs: []netip.Addr{netip.MustParseAddr("100.64.0.9")}}
	backend.mu.Unlock()
	if got := running.Status(context.Background()); got.State != StateError {
		t.Fatalf("query error state %+v", got)
	} else if present, ips := peerIPsJSON(t, got); present {
		t.Fatalf("failed query must omit peerIps, got %v", ips)
	}
}

func TestControllerRunningAndErrors(t *testing.T) {
	c, backend := newController()
	backend.snapshot = Snapshot{BackendState: "Running", TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.9")}, HostName: "node"}
	_ = backend.Start()
	got := c.Status(context.Background())
	if got.State != StateRunning || got.Hostname != "node" || got.TailnetIPs[0] != "100.64.0.9" {
		t.Fatalf("%+v", got)
	}
	if _, err := c.LoginURL(context.Background()); !errors.Is(err, ErrLoginURLUnavailable) {
		t.Fatalf("LoginURL while running: %v", err)
	}
	backend.queryErr = errors.New("local api: " + fixtureAuthKey)
	if got := c.Status(context.Background()); got.State != StateError || strings.Contains(got.Error, "kFIXTURE") {
		t.Fatalf("%+v", got)
	}

	failed, failing := newController()
	failing.startErr = errors.New("start failed for " + fixtureLoginURL)
	_ = failing.Start()
	if got := failed.Status(context.Background()); got.State != StateError || strings.Contains(got.Error, "0123456789abcdef") {
		t.Fatalf("%+v", got)
	}

	c.Lifecycle.Closed()
	if got := c.Status(context.Background()); got.State != StateStopped {
		t.Fatalf("closed: %+v", got)
	}
	if err := c.Login(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("login after close: %v", err)
	}
}

func waitForState(t *testing.T, c *Controller, want State) Status {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := c.Status(context.Background())
		if got.State == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %s, want %s: %+v", got.State, want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestControllerReportsAsyncLoginFailure(t *testing.T) {
	c, backend := newController()
	backend.loginErr = errors.New("control refused: " + fixtureLoginURL)
	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := waitForState(t, c, StateError)
	if got.Error == "" || strings.Contains(got.Error, "0123456789abcdef") {
		t.Fatalf("login failure must be reported redacted: %+v", got)
	}

	// A new attempt clears the previous failure.
	backend.mu.Lock()
	backend.loginErr = nil
	backend.mu.Unlock()
	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for got := c.Status(context.Background()); !got.LoginURLAvailable; got = c.Status(context.Background()) {
		if got.State == StateError || time.Now().After(deadline) {
			t.Fatalf("retry: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLifecycleIgnoresStaleLoginResults(t *testing.T) {
	var life Lifecycle
	first := life.LoginStarted()
	second := life.LoginStarted()
	life.LoginResult(first, errors.New("stale"))
	if got := life.Snapshot().LoginErr; got != nil {
		t.Fatalf("stale result applied: %v", got)
	}
	life.LoginResult(second, errors.New("current"))
	if got := life.Snapshot().LoginErr; got == nil || got.Error() != "current" {
		t.Fatalf("current result: %v", got)
	}
}

func TestControllerReportsAsyncStartFailure(t *testing.T) {
	c, backend := newController()
	backend.startErr = errors.New("listen failed")
	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := waitForState(t, c, StateError); !strings.Contains(got.Error, "listen failed") {
		t.Fatalf("%+v", got)
	}
}

func TestMarkControllerAuth(t *testing.T) {
	for _, secret := range []bool{true, false} {
		var seen bool
		handler := MarkControllerAuth(secret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = ControllerAuthenticated(r.Context())
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		if seen != secret {
			t.Errorf("secret=%v saw %v", secret, seen)
		}
	}
	if ControllerAuthenticated(context.Background()) {
		t.Error("unmarked context authenticated")
	}
}

func TestStatusCarriesConfigInEveryPhase(t *testing.T) {
	config := Config{Hostname: "phone", StateDir: "/data/home/ts", Ephemeral: true}
	life := &Lifecycle{}
	controller := &Controller{Name: "ts", Lifecycle: life, Backend: &fakeBackend{}, Config: config}
	check := func(phase string) {
		t.Helper()
		got := controller.Status(context.Background())
		if got.Config == nil || *got.Config != config {
			t.Fatalf("%s: config %+v", phase, got.Config)
		}
	}
	check("idle")
	life.Starting()
	check("starting")
	life.Finished(errors.New("boom"))
	check("failed")
	life.Closed()
	check("closed")
}
