package api

import (
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"cfa/native/tailscale"

	"github.com/metacubex/chi"
	"github.com/metacubex/http"
	"github.com/metacubex/http/httptest"
)

// Fixture only; not a real login URL.
const fixtureLoginURL = "https://login.example.test/a/fixture0123456789"

type fakeBackend struct {
	mu       sync.Mutex
	snapshot tailscale.Snapshot
}

func (f *fakeBackend) Start() error { return nil }
func (f *fakeBackend) Query(context.Context) (tailscale.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot, nil
}
func (f *fakeBackend) LoginInteractive(context.Context) error { return nil }
func (f *fakeBackend) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshot = tailscale.Snapshot{BackendState: "NeedsLogin"}
	return nil
}

type fakeOutbound struct{ controller *tailscale.Controller }

func (f fakeOutbound) TailscaleController() *tailscale.Controller { return f.controller }

func started(name string, snapshot tailscale.Snapshot) fakeOutbound {
	life := &tailscale.Lifecycle{}
	life.Starting()
	life.ServerStarted()
	life.Finished(nil)
	return fakeOutbound{&tailscale.Controller{Name: name, Lifecycle: life, Backend: &fakeBackend{snapshot: snapshot}}}
}

func withAdapters(t *testing.T, adapters map[string]any) {
	entries := []adapterEntry{}
	for name, adapter := range adapters {
		entries = append(entries, adapterEntry{name: name, adapter: adapter})
	}
	withEntries(t, entries...)
}

// withEntries replaces the adapter source with entries yielded in order, like
// top-level proxies followed by provider members.
func withEntries(t *testing.T, entries ...adapterEntry) {
	previous := adapterSource
	adapterSource = func(yield func(adapterEntry) bool) {
		for _, entry := range entries {
			if !yield(entry) {
				return
			}
		}
	}
	t.Cleanup(func() { adapterSource = previous })
}

func withConfig(outbound fakeOutbound, config tailscale.Config) fakeOutbound {
	outbound.controller.Config = config
	return outbound
}

func newRouter(secret bool) http.Handler {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(tailscale.MarkControllerAuth(secret))
		r.Get(Path, listStatus)
		r.Get(Path+"/status", getStatus)
		r.Post(Path+"/login", login)
		r.Get(Path+"/login-url", loginURL)
		r.Post(Path+"/logout", logout)
	})
	return r
}

func do(t *testing.T, h http.Handler, method, target string) (int, string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body), rec.Result().Header
}

func TestStatusesPeerIPsFollowWhetherTheNetmapWasQueried(t *testing.T) {
	idle := fakeOutbound{&tailscale.Controller{Name: "idle", Lifecycle: &tailscale.Lifecycle{}, Backend: &fakeBackend{}}}
	withEntries(t,
		adapterEntry{name: "up", adapter: started("up", tailscale.Snapshot{
			BackendState: "Running",
			TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.2")},
			PeersQueried: true,
			PeerIPs: []netip.Addr{
				netip.MustParseAddr("100.64.0.9"),
				netip.MustParseAddr("fd7a:115c:a1e0::5"),
				netip.MustParseAddr("100.64.0.2"),
			},
		})},
		adapterEntry{name: "alone", adapter: started("alone", tailscale.Snapshot{
			BackendState: "Running",
			TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.4")},
			PeersQueried: true,
		})},
		adapterEntry{name: "idle", adapter: idle},
	)
	_, body := listOutbounds(t)
	var raw struct {
		Outbounds []map[string]json.RawMessage `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]json.RawMessage{}
	for _, outbound := range raw.Outbounds {
		var name string
		if err := json.Unmarshal(outbound["name"], &name); err != nil {
			t.Fatal(err)
		}
		byName[name] = outbound
	}
	if _, ok := byName["up"]["peerIps"]; !ok {
		t.Fatalf("up omitted peerIps: %s", body)
	}
	if string(byName["up"]["peerIps"]) != `["100.64.0.9","fd7a:115c:a1e0::5"]` {
		t.Fatalf("up peerIps %s", byName["up"]["peerIps"])
	}
	if string(byName["alone"]["peerIps"]) != "[]" {
		t.Fatalf("alone peerIps %s", byName["alone"]["peerIps"])
	}
	if _, ok := byName["idle"]["peerIps"]; ok {
		t.Fatalf("idle status queried no peers but has peerIps: %s", body)
	}
	for _, leaked := range []string{"nodekey", "tskey", "HostName", "login.example", "/a/"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("%q leaked: %s", leaked, body)
		}
	}
}

func TestStatusesListOnlyTailscaleOutbounds(t *testing.T) {
	withAdapters(t, map[string]any{
		"b-ts":   started("b-ts", tailscale.Snapshot{BackendState: "Running"}),
		"a-ts":   started("a-ts", tailscale.Snapshot{BackendState: "NeedsLogin", AuthURL: fixtureLoginURL}),
		"direct": struct{}{},
	})
	code, body, _ := do(t, newRouter(true), http.MethodGet, Path)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	var list listResponse
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Outbounds) != 2 || list.Outbounds[0].Name != "a-ts" || !list.Outbounds[0].LoginURLAvailable {
		t.Fatalf("%+v", list)
	}
	if strings.Contains(body, "/a/") {
		t.Fatalf("status leaks login URL: %s", body)
	}
}

func TestStatusLookupErrors(t *testing.T) {
	withAdapters(t, map[string]any{"direct": struct{}{}})
	h := newRouter(true)
	if code, _, _ := do(t, h, http.MethodGet, Path+"/status"); code != http.StatusBadRequest {
		t.Fatalf("missing name: %d", code)
	}
	if code, _, _ := do(t, h, http.MethodGet, Path+"/status?name=direct"); code != http.StatusNotFound {
		t.Fatalf("non-tailscale: %d", code)
	}
}

func TestLoginURLRequiresSecretAndIsNotCached(t *testing.T) {
	withAdapters(t, map[string]any{
		"ts": started("ts", tailscale.Snapshot{BackendState: "NeedsLogin", AuthURL: fixtureLoginURL}),
	})
	if code, body, _ := do(t, newRouter(false), http.MethodGet, Path+"/login-url?name=ts"); code != http.StatusForbidden || strings.Contains(body, "/a/") {
		t.Fatalf("unauthenticated controller: %d %s", code, body)
	}
	code, body, header := do(t, newRouter(true), http.MethodGet, Path+"/login-url?name=ts")
	if code != http.StatusOK || !strings.Contains(body, fixtureLoginURL) || header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s %v", code, body, header)
	}
}

func TestLoginAndLogout(t *testing.T) {
	withAdapters(t, map[string]any{
		"ts": started("ts", tailscale.Snapshot{BackendState: "Running"}),
	})
	h := newRouter(true)
	if code, body, _ := do(t, h, http.MethodPost, Path+"/login?name=ts"); code != http.StatusAccepted {
		t.Fatalf("login %d %s", code, body)
	}
	if code, body, _ := do(t, h, http.MethodGet, Path+"/login-url?name=ts"); code != http.StatusConflict {
		t.Fatalf("login-url while running %d %s", code, body)
	}
	if code, body, _ := do(t, h, http.MethodPost, Path+"/logout?name=ts"); code != http.StatusNoContent {
		t.Fatalf("logout %d %s", code, body)
	}
	_, body, _ := do(t, h, http.MethodGet, Path+"/status?name=ts")
	if !strings.Contains(body, `"state":"NeedsLogin"`) {
		t.Fatalf("after logout %s", body)
	}
}

func listOutbounds(t *testing.T) (listResponse, string) {
	t.Helper()
	code, body, _ := do(t, newRouter(true), http.MethodGet, Path)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	var list listResponse
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	return list, body
}

func TestStatusesIncludeProviderMembersWithConfig(t *testing.T) {
	local := withConfig(started("local", tailscale.Snapshot{BackendState: "Running"}),
		tailscale.Config{Hostname: "phone", StateDir: "/data/home/ts-local"})
	remote := withConfig(started("remote", tailscale.Snapshot{BackendState: "NeedsLogin"}),
		tailscale.Config{StateDir: "/data/home/tailscale", StateDirDefault: true, HasAuthKey: true, CustomControlURL: true, ControlURLHost: "control.example.test"})
	withEntries(t,
		adapterEntry{name: "local", adapter: local},
		adapterEntry{name: "remote", provider: "subscription", adapter: remote},
	)
	list, body := listOutbounds(t)
	if len(list.Outbounds) != 2 {
		t.Fatalf("%s", body)
	}
	got := list.Outbounds[1]
	if got.Name != "remote" || got.Provider != "subscription" || got.Config == nil ||
		!got.Config.StateDirDefault || !got.Config.HasAuthKey || !got.Config.CustomControlURL ||
		got.Config.ControlURLHost != "control.example.test" {
		t.Fatalf("provider member %+v", got)
	}
	if first := list.Outbounds[0]; first.Provider != "" || first.Config == nil || first.Config.Hostname != "phone" ||
		first.Config.StateDir != "/data/home/ts-local" {
		t.Fatalf("top-level %+v", first)
	}
	if strings.Contains(body, "tskey") || strings.Contains(body, "https://") {
		t.Fatalf("status leaks secrets: %s", body)
	}
}

func TestStatusesReportOneAdapterOnce(t *testing.T) {
	// Mihomo's implicit "default" provider wraps the same top-level proxies.
	ts := started("ts", tailscale.Snapshot{BackendState: "Running"})
	withEntries(t,
		adapterEntry{name: "ts", adapter: ts},
		adapterEntry{name: "ts", provider: "default", adapter: ts},
	)
	list, body := listOutbounds(t)
	if len(list.Outbounds) != 1 || list.Outbounds[0].Provider != "" {
		t.Fatalf("%s", body)
	}
}

func TestStatusesKeepSameNameFromDifferentProviders(t *testing.T) {
	withEntries(t,
		adapterEntry{name: "ts", provider: "b", adapter: started("ts", tailscale.Snapshot{BackendState: "Running"})},
		adapterEntry{name: "ts", provider: "a", adapter: started("ts", tailscale.Snapshot{BackendState: "NeedsLogin"})},
	)
	list, body := listOutbounds(t)
	if len(list.Outbounds) != 2 || list.Outbounds[0].Provider != "a" || list.Outbounds[1].Provider != "b" ||
		list.Outbounds[0].State != tailscale.StateNeedsLogin {
		t.Fatalf("%s", body)
	}
}

func TestLookupSelectsProvider(t *testing.T) {
	withEntries(t,
		adapterEntry{name: "ts", provider: "a", adapter: started("ts", tailscale.Snapshot{BackendState: "Running"})},
		adapterEntry{name: "ts", provider: "b", adapter: started("ts", tailscale.Snapshot{BackendState: "NeedsLogin"})},
		adapterEntry{name: "solo", provider: "a", adapter: started("solo", tailscale.Snapshot{BackendState: "Running"})},
	)
	h := newRouter(true)
	if _, body, _ := do(t, h, http.MethodGet, Path+"/status?name=ts&provider=b"); !strings.Contains(body, `"state":"NeedsLogin"`) ||
		!strings.Contains(body, `"provider":"b"`) {
		t.Fatalf("provider b: %s", body)
	}
	if code, body, _ := do(t, h, http.MethodGet, Path+"/status?name=ts"); code != http.StatusConflict {
		t.Fatalf("ambiguous name without provider: %d %s", code, body)
	}
	if code, body, _ := do(t, h, http.MethodGet, Path+"/status?name=solo"); code != http.StatusOK || !strings.Contains(body, `"provider":"a"`) {
		t.Fatalf("unique provider member without provider: %d %s", code, body)
	}
	if code, _, _ := do(t, h, http.MethodGet, Path+"/status?name=ts&provider=c"); code != http.StatusNotFound {
		t.Fatalf("unknown provider: %d", code)
	}
	if _, err := Lookup("ts", "a"); err != nil {
		t.Fatal(err)
	}
}

func TestLookupPrefersTopLevelOutbound(t *testing.T) {
	withEntries(t,
		adapterEntry{name: "ts", adapter: started("ts", tailscale.Snapshot{BackendState: "Running"})},
		adapterEntry{name: "ts", provider: "a", adapter: started("ts", tailscale.Snapshot{BackendState: "NeedsLogin"})},
	)
	if _, body, _ := do(t, newRouter(true), http.MethodGet, Path+"/status?name=ts"); !strings.Contains(body, `"state":"Running"`) {
		t.Fatalf("%s", body)
	}
}
