package libclash

import (
	"context"
	"time"

	tailscaleapi "cfa/native/tailscale/api"
)

const tailscaleCallTimeout = 8 * time.Second

// QueryTailscaleStatus returns the same JSON as GET /asterisk/tailscale:
// {"outbounds":[{"name","state","loginUrlAvailable","tailnetIps",...}]}.
// It never starts tsnet and never contains a login URL.
func QueryTailscaleStatus() string {
	ctx, cancel := context.WithTimeout(context.Background(), tailscaleCallTimeout)
	defer cancel()
	return marshalJSON(map[string]any{"outbounds": tailscaleapi.Statuses(ctx)})
}

// The functions below select an outbound by name and proxy provider. An empty
// provider selects the top-level outbound, or the only provider member with
// that name; QueryTailscaleStatus reports the provider of every outbound.

// StartTailscaleLogin starts the named tailscale outbound if needed and
// requests an interactive login. It returns immediately; poll the status
// until loginUrlAvailable or Running.
func StartTailscaleLogin(name, provider string) error {
	controller, err := tailscaleapi.Lookup(name, provider)
	if err != nil {
		return err
	}
	return controller.Login(context.Background())
}

// QueryTailscaleLoginURL returns the pending interactive login URL. Callers
// must keep it in memory only and never log or persist it.
func QueryTailscaleLoginURL(name, provider string) (string, error) {
	controller, err := tailscaleapi.Lookup(name, provider)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), tailscaleCallTimeout)
	defer cancel()
	return controller.LoginURL(ctx)
}

// LogoutTailscale deauthorizes the node of the named tailscale outbound.
func LogoutTailscale(name, provider string) error {
	controller, err := tailscaleapi.Lookup(name, provider)
	if err != nil {
		return err
	}
	return controller.Logout(context.Background())
}
