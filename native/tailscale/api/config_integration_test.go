//go:build with_gvisor && !no_tailscale

package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"cfa/native/tailscale"

	"github.com/metacubex/http"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub/executor"
	"github.com/metacubex/mihomo/tunnel"
)

// Exercise the production parser, adapter wrappers and registry rather than
// replacing adapterSource. No listeners or tsnet servers need to start.
func TestRealConfigTailscaleStatus(t *testing.T) {
	for _, smux := range []bool{false, true} {
		name := "plain"
		muxConfig := ""
		if smux {
			name = "smux"
			muxConfig = "    smux: {enabled: true, protocol: smux}\n"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			previousHome := C.Path.HomeDir()
			C.SetHomeDir(home)
			t.Cleanup(func() { C.SetHomeDir(previousHome) })
			cfg, err := executor.ParseWithBytes([]byte(`
proxies:
  - name: TS
    type: tailscale
    hostname: test-phone
    state-dir: ./tailscale-asterisk
    udp: true
` + muxConfig + `
  - {name: plain-direct, type: direct}
  - name: smux-direct
    type: direct
    smux: {enabled: true, protocol: smux}
proxy-groups:
  - name: Proxy
    type: select
    proxies: [TS, DIRECT]
proxy-providers:
  subscription:
    type: inline
    payload:
      - {name: TS, type: tailscale, hostname: provider-phone, state-dir: ./provider-ts}
rules:
  - IP-CIDR,100.64.0.0/10,TS,no-resolve
  - MATCH,DIRECT
`))
			if err != nil {
				t.Fatal(err)
			}
			previousProxies, previousProviders := tunnel.Proxies(), tunnel.Providers()
			tunnel.UpdateProxies(cfg.Proxies, cfg.Providers)
			t.Cleanup(func() {
				tunnel.UpdateProxies(previousProxies, previousProviders)
				for _, provider := range cfg.Providers {
					for _, proxy := range provider.Proxies() {
						_ = proxy.Close()
					}
				}
			})
			proxy := tunnel.Proxies()["TS"]
			if proxy == nil || proxy.Type() != C.Tailscale {
				t.Fatal("TS is missing from the running proxy registry")
			}
			t.Logf("running TS adapter: %T", proxy.Adapter())
			for range 2 {
				list, body := listOutbounds(t)
				if len(list.Outbounds) != 2 {
					t.Fatalf("want top-level TS and subscription TS, with default deduplicated; got %s", body)
				}
				for i, wantProvider := range []string{"", "subscription"} {
					got := list.Outbounds[i]
					if got.Name != "TS" || got.Provider != wantProvider || got.State != tailscale.StateIdle {
						t.Fatalf("unexpected outbound identity/state: %+v", got)
					}
					controller, err := Lookup("TS", wantProvider)
					if err != nil || controller == nil {
						t.Fatalf("lookup provider %q: %v", wantProvider, err)
					}
				}
				local := list.Outbounds[0]
				if local.Config == nil || local.Config.Hostname != "test-phone" ||
					local.Config.StateDir != filepath.Join(home, "tailscale-asterisk") {
					t.Fatalf("effective config missing: %+v", local.Config)
				}
				if code, _, _ := do(t, newRouter(true), http.MethodGet, Path+"/status?name=TS"); code != http.StatusOK {
					t.Fatalf("top-level status: HTTP %d", code)
				}
				for _, name := range []string{"plain-direct", "smux-direct"} {
					if _, err := Lookup(name, ""); !errors.Is(err, ErrNotFound) {
						t.Fatalf("non-Tailscale %s resolved through a wrapper: %v", name, err)
					}
				}
			}
			for _, dir := range []string{"tailscale-asterisk", "provider-ts"} {
				if _, err := os.Stat(filepath.Join(home, dir)); !os.IsNotExist(err) {
					t.Fatalf("status poll created tsnet state directory %s: %v", dir, err)
				}
			}
		})
	}
}
