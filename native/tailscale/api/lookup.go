package api

import (
	"context"
	"errors"
	"sort"

	"cfa/native/tailscale"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/tunnel"
)

// ErrNotFound means no tailscale outbound has the requested name.
var ErrNotFound = errors.New("tailscale outbound not found")

// ErrAmbiguous means several proxy providers supply a tailscale outbound with
// the requested name and the request did not name a provider.
var ErrAmbiguous = errors.New("tailscale outbound name is ambiguous, pass the provider")

// adapterEntry is one outbound adapter of the running configuration. provider
// is empty for top-level proxies.
type adapterEntry struct {
	name     string
	provider string
	adapter  any
}

// adapterSource yields every configured outbound adapter: top-level proxies
// first, then the members of each proxy provider in provider-name order.
// Tests replace it.
var adapterSource = func(yield func(adapterEntry) bool) {
	for name, proxy := range tunnel.Proxies() {
		if !yield(adapterEntry{name: name, adapter: adapterOf(proxy)}) {
			return
		}
	}
	providers := tunnel.Providers()
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, proxy := range providers[name].Proxies() {
			if proxy == nil {
				continue
			}
			if !yield(adapterEntry{name: proxy.Name(), provider: name, adapter: adapterOf(proxy)}) {
				return
			}
		}
	}
}

func adapterOf(proxy C.Proxy) any {
	if proxy == nil {
		return nil
	}
	return proxy.Adapter()
}

type outbound struct {
	provider   string
	controller *tailscale.Controller
}

// outbounds returns every tailscale outbound once. The same adapter can be
// reachable twice (Mihomo's implicit "default" provider wraps the top-level
// proxies); the first, top-level, occurrence wins.
func outbounds() []outbound {
	seen := map[*tailscale.Lifecycle]bool{}
	found := []outbound{}
	adapterSource(func(entry adapterEntry) bool {
		provider, ok := entry.adapter.(tailscale.Provider)
		if !ok {
			return true
		}
		controller := provider.TailscaleController()
		if controller == nil || controller.Lifecycle == nil || seen[controller.Lifecycle] {
			return true
		}
		seen[controller.Lifecycle] = true
		found = append(found, outbound{provider: entry.provider, controller: controller})
		return true
	})
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].controller.Name != found[j].controller.Name {
			return found[i].controller.Name < found[j].controller.Name
		}
		return found[i].provider < found[j].provider
	})
	return found
}

// Lookup returns the controller of the tailscale outbound named name in
// provider. An empty provider selects the top-level outbound, or the only
// provider member with that name.
func Lookup(name, provider string) (*tailscale.Controller, error) {
	found, err := lookupOutbound(name, provider)
	if err != nil {
		return nil, err
	}
	return found.controller, nil
}

func lookupOutbound(name, provider string) (outbound, error) {
	var candidates []outbound
	for _, candidate := range outbounds() {
		if candidate.controller.Name != name {
			continue
		}
		if candidate.provider == provider {
			return candidate, nil
		}
		candidates = append(candidates, candidate)
	}
	if provider != "" || len(candidates) == 0 {
		return outbound{}, ErrNotFound
	}
	if len(candidates) > 1 {
		return outbound{}, ErrAmbiguous
	}
	return candidates[0], nil
}

// Statuses reports every tailscale outbound, sorted by name and provider.
func Statuses(ctx context.Context) []tailscale.Status {
	statuses := []tailscale.Status{}
	for _, candidate := range outbounds() {
		statuses = append(statuses, statusOf(ctx, candidate))
	}
	return statuses
}

func statusOf(ctx context.Context, candidate outbound) tailscale.Status {
	status := candidate.controller.Status(ctx)
	status.Provider = candidate.provider
	return status
}
