// Package apply connects the inbound-forward registry to Mihomo's running
// configuration. The patched executor.ApplyConfig calls Reconcile after
// tunnel.OnRunning, so forwards follow every profile (re)load. It is separate
// from native/tailscale/api because that package imports hub/route, which
// imports the executor.
package apply

import (
	"sort"

	"cfa/native/tailscale/inbound"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/tunnel"
)

// Entries lists every tailscale outbound of the running configuration that
// exposes an inbound controller: top-level proxies first, then proxy-provider
// members in provider-name order. Mihomo's implicit "default" provider
// re-lists the top-level proxies; those duplicates are dropped.
func Entries() []inbound.Entry {
	var entries []inbound.Entry
	seen := map[*inbound.Controller]bool{}
	add := func(name, provider string, proxy C.Proxy) {
		if proxy == nil {
			return
		}
		source, ok := proxy.Adapter().(inbound.Provider)
		if !ok {
			return
		}
		controller := source.InboundController()
		if controller == nil || seen[controller] {
			return
		}
		seen[controller] = true
		entries = append(entries, inbound.Entry{Name: name, Provider: provider, TopLevel: provider == "", Controller: controller})
	}
	proxies := tunnel.Proxies()
	names := make([]string, 0, len(proxies))
	for name := range proxies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		add(name, "", proxies[name])
	}
	providers := tunnel.Providers()
	providerNames := make([]string, 0, len(providers))
	for name := range providers {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames)
	for _, providerName := range providerNames {
		for _, proxy := range providers[providerName].Proxies() {
			if proxy != nil {
				add(proxy.Name(), providerName, proxy)
			}
		}
	}
	return entries
}

// Reconcile applies the running configuration to the process-wide registry.
func Reconcile() {
	inbound.Default.Reconcile(Entries())
}
