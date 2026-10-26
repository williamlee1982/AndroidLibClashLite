// Package tailscale reports typed state for Mihomo tailscale outbounds.
//
// It deliberately has no Mihomo or tsnet imports: the outbound overlay adapts a
// running tsnet server to Backend, and this package owns lifecycle tracking,
// state mapping and secret redaction so they can be unit tested in isolation.
// Status never carries an auth URL; LoginURL returns it on demand only.
package tailscale

import (
	"net/netip"
	"sort"
	"strings"
)

// State is the stable, typed backend state exposed to clients.
type State string

const (
	// StateIdle means the outbound exists but tsnet has not started yet.
	// Mihomo starts tsnet lazily on the first dial or DNS query.
	StateIdle             State = "Idle"
	StateStarting         State = "Starting"
	StateNeedsLogin       State = "NeedsLogin"
	StateNeedsMachineAuth State = "NeedsMachineAuth"
	StateRunning          State = "Running"
	StateStopped          State = "Stopped"
	StateError            State = "Error"
)

// Status is the JSON contract shared by the Clash API route and JNI export.
type Status struct {
	Name              string   `json:"name"`
	State             State    `json:"state"`
	LoginURLAvailable bool     `json:"loginUrlAvailable"`
	TailnetIPs        []string `json:"tailnetIps"`
	Hostname          string   `json:"hostname,omitempty"`
	DNSName           string   `json:"dnsName,omitempty"`
	MagicDNSSuffix    string   `json:"magicDnsSuffix,omitempty"`
	Health            []string `json:"health,omitempty"`
	Error             string   `json:"error,omitempty"`
	// Provider names the proxy provider that supplied the outbound; empty for
	// outbounds declared in the top-level proxies list.
	Provider string `json:"provider,omitempty"`
	// Config is the effective, non-secret configuration of the outbound.
	Config *Config `json:"config,omitempty"`
	// InboundForwards reports the outbound's inbound-forwards. Absent when
	// the outbound declares none (and on cores without the feature).
	InboundForwards []InboundForward `json:"inboundForwards,omitempty"`
	// PeerIPs is nil when this status did not query the netmap, and the JSON
	// field is then omitted. A non-nil slice, including an empty one, means
	// the netmap was read: other nodes' Tailscale addresses, never this
	// node's, with no names, keys, or endpoints. The app treats omission as
	// "unknown" and an empty array as "no other devices".
	PeerIPs *[]string `json:"peerIps,omitempty"`
}

// InboundForward is the state of one tailnet-to-local TCP forward.
type InboundForward struct {
	Name        string `json:"name"`
	Network     string `json:"network"`
	Listen      int    `json:"listen"`
	Target      string `json:"target"`
	State       string `json:"state"` // Listening, Waiting or Error
	Error       string `json:"error,omitempty"`
	ActiveConns int    `json:"activeConns"`
	TotalConns  uint64 `json:"totalConns"`
	Rejected    uint64 `json:"rejected"`
}

// Config summarizes the effective outbound options after providers and
// override scripts were applied. It never carries the auth key or the full
// control URL.
type Config struct {
	// Hostname is the configured hostname option, which may differ from the
	// live Hostname reported by tsnet.
	Hostname string `json:"hostname,omitempty"`
	// StateDir is the resolved absolute tsnet state directory.
	StateDir string `json:"stateDir,omitempty"`
	// StateDirDefault reports that StateDir is Mihomo's implicit default.
	StateDirDefault bool `json:"stateDirDefault"`
	Ephemeral       bool `json:"ephemeral"`
	HasAuthKey      bool `json:"hasAuthKey"`
	// CustomControlURL reports a control-url option; ControlURLHost is its
	// host, empty when the URL does not parse.
	CustomControlURL bool   `json:"customControlUrl"`
	ControlURLHost   string `json:"controlUrlHost,omitempty"`
}

// Snapshot is the subset of ipnstate.Status the mapping needs.
type Snapshot struct {
	BackendState   string
	AuthURL        string
	TailscaleIPs   []netip.Addr
	HostName       string
	DNSName        string
	MagicDNSSuffix string
	Health         []string
	// PeersQueried is true when PeerIPs was filled from a status that
	// included the netmap. PeerIPs may be empty. SelfTailscaleIPs are this
	// node's addresses that are not already in TailscaleIPs; both are
	// excluded from the reported peer list.
	PeersQueried     bool
	PeerIPs          []netip.Addr
	SelfTailscaleIPs []netip.Addr
}

// FromSnapshot maps a tsnet status snapshot to Status. The auth URL only
// contributes the LoginURLAvailable flag.
func FromSnapshot(name string, snapshot Snapshot) Status {
	status := Status{
		Name:              name,
		LoginURLAvailable: validLoginURL(snapshot.AuthURL) == nil,
		TailnetIPs:        []string{},
		Hostname:          strings.TrimSpace(snapshot.HostName),
		DNSName:           strings.TrimSuffix(strings.TrimSpace(snapshot.DNSName), "."),
		MagicDNSSuffix:    strings.TrimSuffix(strings.TrimSpace(snapshot.MagicDNSSuffix), "."),
	}
	switch snapshot.BackendState {
	case "NoState", "Starting", "":
		status.State = StateStarting
	case "NeedsLogin":
		status.State = StateNeedsLogin
	case "NeedsMachineAuth":
		status.State = StateNeedsMachineAuth
	case "Running":
		status.State = StateRunning
	case "Stopped":
		status.State = StateStopped
	case "InUseOtherUser":
		status.State = StateError
		status.Error = "tailscale state is in use by another user"
	default:
		status.State = StateError
		status.Error = "unknown tailscale backend state"
	}
	if status.State != StateNeedsLogin {
		status.LoginURLAvailable = false
	}
	addresses := make([]netip.Addr, 0, len(snapshot.TailscaleIPs))
	for _, address := range snapshot.TailscaleIPs {
		if address.IsValid() {
			addresses = append(addresses, address.Unmap())
		}
	}
	sortTailscaleIPs(addresses)
	for _, address := range addresses {
		status.TailnetIPs = append(status.TailnetIPs, address.String())
	}
	if snapshot.PeersQueried {
		// Pointer to a non-nil slice, so an empty report is [] and not omitted.
		peerIPs := peerIPStrings(snapshot)
		status.PeerIPs = &peerIPs
	}
	for _, warning := range snapshot.Health {
		if warning = strings.TrimSpace(Redact(warning)); warning != "" {
			status.Health = append(status.Health, warning)
		}
	}
	return status
}

// ErrorStatus reports a failed lifecycle or query with a redacted message.
func ErrorStatus(name string, err error) Status {
	message := "tailscale error"
	if err != nil {
		message = Redact(err.Error())
	}
	return Status{Name: name, State: StateError, TailnetIPs: []string{}, Error: message}
}

// Ranges Tailscale assigns node addresses from. The ChromeOS VM range sits
// inside the IPv4 range but is not used for node addresses; tsaddr.IsTailscaleIP
// excludes it the same way. 100.100.100.100 (the local service address) is
// inside the IPv4 range and is kept if a peer status actually lists it.
var (
	tailscaleIPv4 = netip.MustParsePrefix("100.64.0.0/10")
	chromeOSVM    = netip.MustParsePrefix("100.115.92.0/23")
	tailscaleIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

func isTailscaleAssignedIP(ip netip.Addr) bool {
	if ip.Is4() {
		return tailscaleIPv4.Contains(ip) && !chromeOSVM.Contains(ip)
	}
	return ip.Is6() && tailscaleIPv6.Contains(ip)
}

func sortTailscaleIPs(addresses []netip.Addr) {
	sort.SliceStable(addresses, func(i, j int) bool {
		// IPv4 first, then natural order, so clients can display a primary IP.
		if addresses[i].Is4() != addresses[j].Is4() {
			return addresses[i].Is4()
		}
		return addresses[i].Less(addresses[j])
	})
}

// peerIPStrings is the peerIps array for a snapshot whose netmap was queried.
// The result is never nil. This node's addresses, addresses outside the
// Tailscale ranges, and duplicates are dropped. Offline peers are not a
// concept here: every address the caller supplies is a netmap address.
func peerIPStrings(snapshot Snapshot) []string {
	self := map[netip.Addr]struct{}{}
	for _, address := range append(append([]netip.Addr{}, snapshot.TailscaleIPs...), snapshot.SelfTailscaleIPs...) {
		if !address.IsValid() {
			continue
		}
		self[address.Unmap().WithZone("")] = struct{}{}
	}
	selected := make([]netip.Addr, 0, len(snapshot.PeerIPs))
	seen := map[netip.Addr]struct{}{}
	for _, address := range snapshot.PeerIPs {
		if !address.IsValid() {
			continue
		}
		address = address.Unmap().WithZone("")
		if !isTailscaleAssignedIP(address) {
			continue
		}
		if _, ok := self[address]; ok {
			continue
		}
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		selected = append(selected, address)
	}
	sortTailscaleIPs(selected)
	formatted := make([]string, len(selected))
	for i, address := range selected {
		formatted[i] = address.String()
	}
	return formatted
}
