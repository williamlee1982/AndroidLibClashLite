package inbound

import (
	"strings"
	"testing"
)

// Catch missing normalization and policy validation before any runtime starts.
func TestParseForwards(t *testing.T) {
	tests := []struct {
		name string
		raw  []RawForward
		lan  bool
		want string
	}{
		{name: "defaults", raw: []RawForward{{Listen: 20022, Target: "127.0.0.1:20022"}}},
		{name: "ipv6", raw: []RawForward{{Listen: 20022, Target: "[::1]:20022"}}},
		{name: "loopback-range", raw: []RawForward{{Listen: 20022, Target: "127.23.45.67:20022"}}},
		{name: "localhost", raw: []RawForward{{Listen: 20022, Target: "localhost:20022"}}, want: "[0]: target must be an IP literal"},
		{name: "hostname", raw: []RawForward{{Listen: 20022, Target: "example.test:20022"}}, want: "target must be an IP literal"},
		{name: "zero", raw: []RawForward{{Target: "127.0.0.1:20022"}}, want: "[0]: listen must be 1-65535"},
		{name: "negative", raw: []RawForward{{Listen: -1, Target: "127.0.0.1:20022"}}, want: "listen must be 1-65535"},
		{name: "overflow", raw: []RawForward{{Listen: 65536, Target: "127.0.0.1:20022"}}, want: "listen must be 1-65535"},
		{name: "duplicate", raw: []RawForward{{Listen: 20022, Target: "127.0.0.1:20022"}, {Listen: 20022, Target: "127.0.0.1:20023"}}, want: "[1]: duplicate listen"},
		{name: "udp", raw: []RawForward{{Listen: 20022, Target: "127.0.0.1:20022", Network: "udp"}}, want: "udp inbound forwards are not supported yet"},
		{name: "other-network", raw: []RawForward{{Listen: 20022, Target: "127.0.0.1:20022", Network: "tcp6"}}, want: "only tcp"},
		{name: "lan-rejected", raw: []RawForward{{Listen: 20022, Target: "100.101.102.103:20022"}}, want: "allow-lan-target"},
		{name: "lan-allowed", raw: []RawForward{{Listen: 20022, Target: "100.101.102.103:20022"}}, lan: true},
		{name: "unspecified", raw: []RawForward{{Listen: 20022, Target: "0.0.0.0:20022"}}, lan: true, want: "unicast"},
		{name: "multicast", raw: []RawForward{{Listen: 20022, Target: "224.0.0.1:20022"}}, lan: true, want: "unicast"},
		{name: "broadcast", raw: []RawForward{{Listen: 20022, Target: "255.255.255.255:20022"}}, lan: true, want: "unicast"},
		{name: "port-zero", raw: []RawForward{{Listen: 20022, Target: "127.0.0.1:0"}}, want: "target port must be 1-65535"},
		{name: "mapped-loopback", raw: []RawForward{{Listen: 20022, Target: "[::ffff:127.0.0.1]:20022"}}},
		{name: "mapped-unspecified", raw: []RawForward{{Listen: 20022, Target: "[::ffff:0.0.0.0]:20022"}}, lan: true, want: "unicast"},
		{name: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.raw, tt.lan)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("want %q, got %v", tt.want, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.raw) || got.HasForwards() != (len(tt.raw) > 0) {
				t.Fatalf("forwards %v", got)
			}
			if len(got) > 0 && (got[0].Network != "tcp" || got[0].Name != "20022" || got[0].Listen != 20022 || got[0].Target.Port() != 20022) {
				t.Fatalf("normalization: %+v", got)
			}
		})
	}
}
