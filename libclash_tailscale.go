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

// StartTailscaleLogin starts the named tailscale outbound if needed and
// requests an interactive login. It returns immediately; poll the status
// until loginUrlAvailable or Running.
func StartTailscaleLogin(name string) error {
	controller, err := tailscaleapi.Lookup(name)
	if err != nil {
		return err
	}
	return controller.Login(context.Background())
}

// QueryTailscaleLoginURL returns the pending interactive login URL. Callers
// must keep it in memory only and never log or persist it.
func QueryTailscaleLoginURL(name string) (string, error) {
	controller, err := tailscaleapi.Lookup(name)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), tailscaleCallTimeout)
	defer cancel()
	return controller.LoginURL(ctx)
}

// LogoutTailscale deauthorizes the node of the named tailscale outbound.
func LogoutTailscale(name string) error {
	controller, err := tailscaleapi.Lookup(name)
	if err != nil {
		return err
	}
	return controller.Logout(context.Background())
}
