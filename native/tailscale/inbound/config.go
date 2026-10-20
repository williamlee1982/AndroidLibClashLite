package inbound

import (
	"fmt"
	"net/netip"
)

// RawForward is the configuration representation decoded from a tailscale
// proxy. Listen is signed so malformed and out-of-range YAML values can be
// reported as validation errors instead of overflowing a uint16.
type RawForward struct {
	Listen  int    `yaml:"listen" json:"listen"`
	Target  string `yaml:"target" json:"target"`
	Network string `yaml:"network,omitempty" json:"network,omitempty"`
	Name    string `yaml:"name,omitempty" json:"name,omitempty"`
}

// Forward is a validated inbound TCP forward.
type Forward struct {
	Name    string
	Network string
	Listen  uint16
	Target  netip.AddrPort
}

// Forwards is the validated set attached to one outbound.
type Forwards []Forward

// HasForwards reports whether at least one forward is configured.
func (f Forwards) HasForwards() bool { return len(f) != 0 }

// Parse validates and normalizes inbound-forward configuration.
func Parse(raw []RawForward, allowLanTarget bool) (Forwards, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	forwards := make(Forwards, 0, len(raw))
	seen := make(map[int]struct{}, len(raw))
	for i, item := range raw {
		if item.Listen < 1 || item.Listen > 65535 {
			return nil, fmt.Errorf("inbound forward[%d]: listen must be 1-65535", i)
		}
		if _, exists := seen[item.Listen]; exists {
			return nil, fmt.Errorf("inbound forward[%d]: duplicate listen %d", i, item.Listen)
		}
		seen[item.Listen] = struct{}{}

		network := item.Network
		if network == "" {
			network = "tcp"
		}
		if network == "udp" {
			return nil, fmt.Errorf("inbound forward[%d]: udp inbound forwards are not supported yet", i)
		}
		if network != "tcp" {
			return nil, fmt.Errorf("inbound forward[%d]: only tcp inbound forwards are supported", i)
		}

		target, err := netip.ParseAddrPort(item.Target)
		if err != nil || !target.Addr().IsValid() {
			return nil, fmt.Errorf("inbound forward[%d]: target must be an IP literal in ip:port form", i)
		}
		if target.Port() == 0 {
			return nil, fmt.Errorf("inbound forward[%d]: target port must be 1-65535", i)
		}
		addr := target.Addr()
		if addr.IsUnspecified() || addr.IsMulticast() || (!addr.IsLoopback() && !addr.IsGlobalUnicast()) {
			return nil, fmt.Errorf("inbound forward[%d]: target must be a unicast IP address", i)
		}
		if !addr.IsLoopback() && !allowLanTarget {
			return nil, fmt.Errorf("inbound forward[%d]: non-loopback target requires allow-lan-target", i)
		}

		name := item.Name
		if name == "" {
			name = fmt.Sprintf("%d", item.Listen)
		}
		forwards = append(forwards, Forward{
			Name:    name,
			Network: network,
			Listen:  uint16(item.Listen),
			Target:  target,
		})
	}
	return forwards, nil
}
