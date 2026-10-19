//go:build with_gvisor && !no_tailscale

package outbound

// This file is added to Mihomo's outbound package by the AndroidLibClashLite
// overlay. It exposes typed tsnet state for the Asterisk Tailscale API without
// scraping logs. The patched start()/Close() record lifecycle transitions so
// status queries never start tsnet themselves.

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	tailscalestate "cfa/native/tailscale"

	C "github.com/metacubex/mihomo/constant"
)

var tailscaleLifecycles sync.Map // *Tailscale -> *tailscalestate.Lifecycle

func (t *Tailscale) tailscaleLifecycle() *tailscalestate.Lifecycle {
	if life, ok := tailscaleLifecycles.Load(t); ok {
		return life.(*tailscalestate.Lifecycle)
	}
	life, _ := tailscaleLifecycles.LoadOrStore(t, &tailscalestate.Lifecycle{})
	return life.(*tailscalestate.Lifecycle)
}

func tailscaleStarting(t *Tailscale)                 { t.tailscaleLifecycle().Starting() }
func tailscaleServerStarted(t *Tailscale)            { t.tailscaleLifecycle().ServerStarted() }
func tailscaleStartFinished(t *Tailscale, err error) { t.tailscaleLifecycle().Finished(err) }

// tailscaleClosed marks the outbound closed and drops it from the registry so
// replaced outbounds do not leak across profile reloads.
func tailscaleClosed(t *Tailscale) {
	t.tailscaleLifecycle().Closed()
	tailscaleLifecycles.Delete(t)
}

// tailscaleLogLine formats a tsnet log line with login URLs and keys redacted.
func tailscaleLogLine(format string, args ...any) string {
	return tailscalestate.RedactLog(fmt.Sprintf(format, args...))
}

// TailscaleController implements tailscalestate.Provider.
func (t *Tailscale) TailscaleController() *tailscalestate.Controller {
	return &tailscalestate.Controller{
		Name:         t.Name(),
		Lifecycle:    t.tailscaleLifecycle(),
		Backend:      tailscaleBackend{t},
		Background:   t.ctx,
		LoginTimeout: 2 * time.Minute,
		Config:       tailscaleConfig(t.option),
	}
}

// tailscaleConfig summarizes the effective options without secrets: the auth
// key becomes a flag and the control URL is reduced to its host. NewTailscale
// has already resolved StateDir to an absolute path.
func tailscaleConfig(option TailscaleOption) tailscalestate.Config {
	config := tailscalestate.Config{
		Hostname:        strings.TrimSpace(option.Hostname),
		StateDir:        option.StateDir,
		StateDirDefault: option.StateDir == C.Path.Resolve("tailscale"),
		Ephemeral:       option.Ephemeral,
		HasAuthKey:      strings.TrimSpace(option.AuthKey) != "",
	}
	if control := strings.TrimSpace(option.ControlURL); control != "" {
		if parsed, err := url.Parse(control); err == nil {
			config.ControlURLHost = strings.ToLower(parsed.Hostname())
		}
	}
	return config
}

type tailscaleBackend struct{ t *Tailscale }

func (b tailscaleBackend) Start() error { return b.t.start() }

func (b tailscaleBackend) Query(ctx context.Context) (tailscalestate.Snapshot, error) {
	// LocalClient calls Start, which is a completed sync.Once at this point:
	// the controller only queries after the lifecycle saw a started server.
	lc, err := b.t.server.LocalClient()
	if err != nil {
		return tailscalestate.Snapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return tailscalestate.Snapshot{}, err
	}
	snapshot := tailscalestate.Snapshot{
		BackendState: st.BackendState,
		AuthURL:      st.AuthURL,
		TailscaleIPs: st.TailscaleIPs,
		Health:       st.Health,
	}
	if st.Self != nil {
		snapshot.HostName = st.Self.HostName
		snapshot.DNSName = st.Self.DNSName
	}
	if st.CurrentTailnet != nil {
		snapshot.MagicDNSSuffix = st.CurrentTailnet.MagicDNSSuffix
	}
	return snapshot, nil
}

func (b tailscaleBackend) LoginInteractive(ctx context.Context) error {
	lc, err := b.t.server.LocalClient()
	if err != nil {
		return err
	}
	return lc.StartLoginInteractive(ctx)
}

func (b tailscaleBackend) Logout(ctx context.Context) error {
	lc, err := b.t.server.LocalClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return lc.Logout(ctx)
}
