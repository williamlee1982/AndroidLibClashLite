//go:build with_gvisor && !no_tailscale

package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cfa/native/tailscale"
	"cfa/native/tailscale/inbound"
	inboundapply "cfa/native/tailscale/inbound/apply"

	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub/executor"
	"github.com/metacubex/mihomo/tunnel"
)

func useTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	previous := C.Path.HomeDir()
	C.SetHomeDir(home)
	t.Cleanup(func() { C.SetHomeDir(previous) })
	return home
}

func closeAll(cfg *config.Config) {
	for _, proxy := range cfg.Proxies {
		_ = proxy.Close()
	}
	for _, provider := range cfg.Providers {
		for _, proxy := range provider.Proxies() {
			_ = proxy.Close()
		}
	}
}

func TestRealConfigInboundForwardsParsed(t *testing.T) {
	for _, smux := range []bool{false, true} {
		name, muxConfig := "plain", ""
		if smux {
			name, muxConfig = "smux", "    smux: {enabled: true, protocol: smux}\n"
		}
		t.Run(name, func(t *testing.T) {
			home := useTempHome(t)
			cfg, err := executor.ParseWithBytes([]byte(`
proxies:
  - name: TS
    type: tailscale
    hostname: test-phone
    state-dir: ./ts-forward
    allow-lan-target: true
    inbound-forwards:
      - {listen: 20022, target: "127.0.0.1:20022", name: ssh}
      - {listen: 20023, target: "192.168.200.10:20023"}
` + muxConfig + `
  - {name: LAZY, type: tailscale, state-dir: ./ts-lazy}
proxy-providers:
  subscription:
    type: inline
    payload:
      - {name: PTS, type: tailscale, state-dir: ./provider-ts, inbound-forwards: [{listen: 20024, target: "127.0.0.1:20024"}]}
rules:
  - MATCH,DIRECT
`))
			if err != nil {
				t.Fatal(err)
			}
			previousProxies, previousProviders := tunnel.Proxies(), tunnel.Providers()
			tunnel.UpdateProxies(cfg.Proxies, cfg.Providers)
			t.Cleanup(func() {
				tunnel.UpdateProxies(previousProxies, previousProviders)
				closeAll(cfg)
			})

			// Through the autoClose (and SingMux) wrappers.
			provider, ok := tunnel.Proxies()["TS"].Adapter().(inbound.Provider)
			if !ok || provider.InboundController() == nil {
				t.Fatalf("wrapper %T hides InboundController", tunnel.Proxies()["TS"].Adapter())
			}
			controller := provider.InboundController()
			if controller != provider.InboundController() {
				t.Fatal("InboundController is not stable per outbound")
			}
			if len(controller.Forwards) != 2 || controller.Forwards[0].Name != "ssh" ||
				controller.Forwards[1].Name != "20023" ||
				controller.StateDir != filepath.Join(home, "ts-forward") {
				t.Fatalf("controller = %+v", controller)
			}

			byName := map[string]inbound.Entry{}
			for _, entry := range inboundapply.Entries() {
				if _, dup := byName[entry.Name+"/"+entry.Provider]; dup {
					t.Fatalf("duplicate entry %s/%s", entry.Name, entry.Provider)
				}
				byName[entry.Name+"/"+entry.Provider] = entry
			}
			if e := byName["TS/"]; !e.TopLevel || e.Controller != controller {
				t.Fatalf("top-level TS entry = %+v", e)
			}
			if e := byName["LAZY/"]; !e.TopLevel || e.Controller == nil || e.Controller.Forwards.HasForwards() {
				t.Fatalf("LAZY entry = %+v", e)
			}
			if e := byName["PTS/subscription"]; e.TopLevel || e.Controller == nil || len(e.Controller.Forwards) != 1 {
				t.Fatalf("provider entry = %+v", e)
			}

			// Parsing and enumerating never start tsnet.
			list, body := listOutbounds(t)
			for _, got := range list.Outbounds {
				if got.State != tailscale.StateIdle {
					t.Fatalf("outbound started by parse: %s", body)
				}
			}
			for _, dir := range []string{"ts-forward", "ts-lazy", "provider-ts"} {
				if _, err := os.Stat(filepath.Join(home, dir)); !os.IsNotExist(err) {
					t.Fatalf("parse created state dir %s", dir)
				}
			}
		})
	}
}

func TestRealConfigInboundForwardsInvalid(t *testing.T) {
	useTempHome(t)
	for want, forward := range map[string]string{
		"udp inbound forwards are not supported yet": `{listen: 20022, target: "127.0.0.1:22", network: udp}`,
		"requires allow-lan-target":                  `{listen: 20022, target: "192.168.200.10:22"}`,
		"target must be an IP literal":               `{listen: 20022, target: "example.test:22"}`,
		"listen must be 1-65535":                     `{listen: 70000, target: "127.0.0.1:22"}`,
	} {
		_, err := executor.ParseWithBytes([]byte(`
proxies:
  - {name: TS, type: tailscale, state-dir: ./ts, inbound-forwards: [` + forward + `]}
rules: ["MATCH,DIRECT"]
`))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want %q", forward, err, want)
		}
	}
}

func applyYAML(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := executor.ParseWithBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	executor.ApplyConfig(cfg, true)
	return cfg
}

func waitStatus(t *testing.T, name string, ok func(tailscale.Status) bool) tailscale.Status {
	t.Helper()
	var last tailscale.Status
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, status := range Statuses(t.Context()) {
			if status.Name == name && status.Provider == "" {
				last = status
				if ok(status) {
					return status
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("status of %s never matched; last %+v", name, last)
	return last
}

// End to end through the patched executor hook: forwards make ApplyConfig
// start tsnet eagerly; a reload with the same state dir closes the previous
// instance first (D4); an outbound without forwards stays lazy (D5). The
// control URL is unreachable, so the node stays logged out and the forward
// reports Waiting rather than Listening.
func TestApplyConfigEagerStartAndSupersede(t *testing.T) {
	home := useTempHome(t)
	previousProxies, previousProviders := tunnel.Proxies(), tunnel.Providers()
	var applied []*config.Config
	t.Cleanup(func() {
		tunnel.UpdateProxies(previousProxies, previousProviders)
		inboundapply.Reconcile()
		for _, cfg := range applied {
			closeAll(cfg)
		}
	})
	forwarding := `
proxies:
  - name: TS
    type: tailscale
    state-dir: ./ts-eager
    control-url: http://127.0.0.1:1
    inbound-forwards: [{listen: 20022, target: "127.0.0.1:20022", name: ssh}]
rules: ["MATCH,DIRECT"]
`
	applied = append(applied, applyYAML(t, forwarding))
	first := tunnel.Proxies()["TS"].Adapter().(tailscale.Provider).TailscaleController()
	status := waitStatus(t, "TS", func(s tailscale.Status) bool {
		return s.State != tailscale.StateIdle && s.State != tailscale.StateStarting && len(s.InboundForwards) == 1
	})
	if status.State != tailscale.StateNeedsLogin || status.InboundForwards[0].State != string(inbound.StateWaiting) ||
		status.InboundForwards[0].Listen != 20022 || status.InboundForwards[0].Name != "ssh" {
		t.Fatalf("eager status = %+v", status)
	}
	if _, err := os.Stat(filepath.Join(home, "ts-eager")); err != nil {
		t.Fatalf("eager start did not create its state dir: %v", err)
	}

	// Reload with forwards removed: the old identity is closed, the new
	// outbound stays lazy.
	applied = append(applied, applyYAML(t, strings.Replace(forwarding,
		`    inbound-forwards: [{listen: 20022, target: "127.0.0.1:20022", name: ssh}]
`, "", 1)))
	if phase := first.Lifecycle.Snapshot().Phase; phase != tailscale.PhaseClosed {
		t.Fatalf("superseded outbound phase = %v, want closed", phase)
	}
	if first.InboundForwards != nil && len(first.InboundForwards()) != 0 {
		t.Fatalf("closed outbound still reports forwards: %+v", first.InboundForwards())
	}
	lazy := waitStatus(t, "TS", func(tailscale.Status) bool { return true })
	if lazy.State != tailscale.StateIdle || lazy.InboundForwards != nil {
		t.Fatalf("outbound without forwards started or reports forwards: %+v", lazy)
	}
}
