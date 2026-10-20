//go:build with_gvisor && !no_tailscale

package outbound

// This file is added to Mihomo's outbound package by the AndroidLibClashLite
// overlay. It implements inbound-forwards: TCP listeners on the outbound's
// tsnet node that forward to local ip:port targets. Parsing only validates;
// the registry (cfa/native/tailscale/inbound) starts forwards from the
// patched executor.ApplyConfig.

import (
	"context"
	"net"
	"sync"
	"time"

	tailscalestate "cfa/native/tailscale"
	"cfa/native/tailscale/inbound"

	"github.com/metacubex/mihomo/component/dialer"
	"github.com/metacubex/mihomo/log"
)

// TailscaleInboundForward is one inbound-forwards entry of a tailscale proxy.
type TailscaleInboundForward struct {
	Listen  int    `proxy:"listen"`
	Target  string `proxy:"target"`
	Network string `proxy:"network,omitempty"`
	Name    string `proxy:"name,omitempty"`
}

var tailscaleInbounds sync.Map // *Tailscale -> *inbound.Controller

func tailscaleParseInbound(option TailscaleOption) (inbound.Forwards, error) {
	raw := make([]inbound.RawForward, 0, len(option.InboundForwards))
	for _, forward := range option.InboundForwards {
		raw = append(raw, inbound.RawForward{Listen: forward.Listen, Target: forward.Target,
			Network: forward.Network, Name: forward.Name})
	}
	return inbound.Parse(raw, option.AllowLanTarget)
}

// tailscaleValidateInbound runs first in NewTailscale so an invalid forward
// fails the profile load with a clear message.
func tailscaleValidateInbound(option TailscaleOption) error {
	_, err := tailscaleParseInbound(option)
	return err
}

// InboundController implements inbound.Provider. It returns one stable
// controller per outbound instance, or nil once the outbound is closed.
func (t *Tailscale) InboundController() *inbound.Controller {
	if t.ctx.Err() != nil {
		return nil
	}
	if c, ok := tailscaleInbounds.Load(t); ok {
		return c.(*inbound.Controller)
	}
	forwards, _ := tailscaleParseInbound(t.option) // validated by NewTailscale
	name := t.Name()
	c := &inbound.Controller{
		StateDir: t.option.StateDir,
		Forwards: forwards,
		// Eager start (D5) without blocking ApplyConfig; failures surface
		// through the lifecycle in the status API.
		Start: func() error {
			go func() { _ = t.start() }()
			return nil
		},
		Ready: func() bool { return tailscaleInboundReady(t) },
		Listen: func(network, addr string) (net.Listener, error) {
			// Ready guarantees start() completed; tsnet's own Start inside
			// Listen is then a no-op, so Close still sees serverStarted.
			if t.ctx.Err() != nil {
				return nil, net.ErrClosed
			}
			return t.server.Listen(network, addr)
		},
		// Targets are local. Mihomo's dialer applies the core's routing mark
		// and Android socket protection so the dial cannot loop back into the
		// tunnel, and it skips interface binding for loopback targets.
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
		Close: t.Close,
		Logger: func(format string, args ...any) {
			log.Infoln("[Tailscale](%s) %s", name, tailscaleLogLine(format, args...))
		},
	}
	actual, _ := tailscaleInbounds.LoadOrStore(t, c)
	if t.ctx.Err() != nil { // closed concurrently: do not leak the entry
		tailscaleInbounds.Delete(t)
		inbound.Default.Forget(actual.(*inbound.Controller))
		return nil
	}
	return actual.(*inbound.Controller)
}

// tailscaleInboundReady reports a started, logged-in node. Listeners register
// only then, so "Listening" means reachable from the tailnet.
func tailscaleInboundReady(t *Tailscale) bool {
	if t.ctx.Err() != nil {
		return false
	}
	life, ok := tailscaleLifecycles.Load(t)
	if !ok {
		return false
	}
	snapshot := life.(*tailscalestate.Lifecycle).Snapshot()
	if !snapshot.ServerStarted || snapshot.Phase == tailscalestate.PhaseClosed {
		return false
	}
	ctx, cancel := context.WithTimeout(t.ctx, 3*time.Second)
	defer cancel()
	status, err := tailscaleBackend{t}.Query(ctx)
	return err == nil && status.BackendState == "Running"
}

// tailscaleInboundClosed stops the outbound's listeners and forwarded
// connections. Called from the patched Close through tailscaleClosed.
func tailscaleInboundClosed(t *Tailscale) {
	if c, ok := tailscaleInbounds.LoadAndDelete(t); ok {
		inbound.Default.Forget(c.(*inbound.Controller))
	}
}

// tailscaleInboundStatus reports the outbound's forwards for the status API.
func tailscaleInboundStatus(t *Tailscale) []tailscalestate.InboundForward {
	c, ok := tailscaleInbounds.Load(t)
	if !ok {
		return nil
	}
	snapshots := inbound.Default.Snapshot(c.(*inbound.Controller))
	if len(snapshots) == 0 {
		return nil
	}
	out := make([]tailscalestate.InboundForward, 0, len(snapshots))
	for _, s := range snapshots {
		out = append(out, tailscalestate.InboundForward{
			Name: s.Name, Network: s.Network, Listen: int(s.Listen), Target: s.Target,
			State: string(s.State), Error: s.Error, ActiveConns: s.ActiveConns,
			TotalConns: s.TotalConns, Rejected: s.Rejected,
		})
	}
	return out
}

// The proxy wrappers built by ParseProxy embed only the ProxyAdapter
// interface, which hides this extension method; forward it explicitly.
func (p *autoCloseProxyAdapter) InboundController() *inbound.Controller {
	if provider, ok := p.ProxyAdapter.(inbound.Provider); ok {
		return provider.InboundController()
	}
	return nil
}

func (s *SingMux) InboundController() *inbound.Controller {
	if provider, ok := s.ProxyAdapter.(inbound.Provider); ok {
		return provider.InboundController()
	}
	return nil
}
