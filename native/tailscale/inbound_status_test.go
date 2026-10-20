package tailscale

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestStatusIncludesInboundForwards(t *testing.T) {
	c, _ := newController()
	forwards := []InboundForward{{Name: "ssh", Network: "tcp", Listen: 20022, Target: "127.0.0.1:20022",
		State: "Listening", ActiveConns: 1, TotalConns: 3}}
	c.InboundForwards = func() []InboundForward { return forwards }
	got := c.Status(context.Background())
	if len(got.InboundForwards) != 1 || got.InboundForwards[0] != forwards[0] {
		t.Fatalf("inbound forwards = %+v", got.InboundForwards)
	}
	body, _ := json.Marshal(got)
	for _, want := range []string{`"inboundForwards":[`, `"listen":20022`, `"target":"127.0.0.1:20022"`,
		`"state":"Listening"`, `"activeConns":1`, `"totalConns":3`, `"network":"tcp"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("%s missing from %s", want, body)
		}
	}
}

// The app treats an absent field as "core without inbound support"; an
// outbound without forwards must omit it rather than send [].
func TestStatusOmitsInboundForwardsWhenNone(t *testing.T) {
	for _, source := range []func() []InboundForward{nil, func() []InboundForward { return nil }} {
		c, _ := newController()
		c.InboundForwards = source
		body, _ := json.Marshal(c.Status(context.Background()))
		if strings.Contains(string(body), "inboundForwards") {
			t.Fatalf("unexpected field: %s", body)
		}
	}
}

func TestStatusRedactsInboundForwards(t *testing.T) {
	c, _ := newController()
	c.InboundForwards = func() []InboundForward {
		return []InboundForward{{Name: fixtureAuthKey, Listen: 20022, Target: "127.0.0.1:20022",
			State: "Error", Error: "dial failed " + fixtureLoginURL}}
	}
	body, _ := json.Marshal(c.Status(context.Background()))
	if strings.Contains(string(body), fixtureAuthKey) || strings.Contains(string(body), fixtureLoginURL) {
		t.Fatalf("secret leaked: %s", body)
	}
}
