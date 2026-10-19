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

// adapterSource yields every configured outbound adapter, including provider
// members. Tests replace it.
var adapterSource = func(yield func(name string, adapter any) bool) {
	for name, proxy := range tunnel.Proxies() {
		if !yield(name, adapterOf(proxy)) {
			return
		}
	}
	for _, provider := range tunnel.Providers() {
		for _, proxy := range provider.Proxies() {
			if !yield(proxy.Name(), adapterOf(proxy)) {
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

// Lookup returns the controller of the tailscale outbound named name.
func Lookup(name string) (*tailscale.Controller, error) {
	var found *tailscale.Controller
	adapterSource(func(candidate string, adapter any) bool {
		if candidate != name {
			return true
		}
		if provider, ok := adapter.(tailscale.Provider); ok {
			found = provider.TailscaleController()
			return false
		}
		return true
	})
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

// Statuses reports every tailscale outbound, sorted by name.
func Statuses(ctx context.Context) []tailscale.Status {
	seen := map[string]bool{}
	statuses := []tailscale.Status{}
	adapterSource(func(name string, adapter any) bool {
		provider, ok := adapter.(tailscale.Provider)
		if !ok || seen[name] {
			return true
		}
		seen[name] = true
		statuses = append(statuses, provider.TailscaleController().Status(ctx))
		return true
	})
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses
}
