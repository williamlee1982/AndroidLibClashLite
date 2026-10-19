package api

import (
	"context"
	"encoding/json"
	"io"
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
	previous := adapterSource
	adapterSource = func(yield func(string, any) bool) {
		for name, adapter := range adapters {
			if !yield(name, adapter) {
				return
			}
		}
	}
	t.Cleanup(func() { adapterSource = previous })
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
