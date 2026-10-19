package tailscale

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// Backend adapts one tsnet server. Query, LoginInteractive and Logout are only
// called after the lifecycle reports a started server.
type Backend interface {
	// Start runs the outbound's own lazy start (sync.Once inside Mihomo).
	Start() error
	Query(ctx context.Context) (Snapshot, error)
	LoginInteractive(ctx context.Context) error
	Logout(ctx context.Context) error
}

// Controller is what a tailscale outbound exposes to the API and JNI layers.
type Controller struct {
	Name      string
	Lifecycle *Lifecycle
	Backend   Backend
	// Background bounds asynchronous login work; it is the outbound's context.
	Background context.Context
	// LoginTimeout bounds the asynchronous start and login request.
	LoginTimeout time.Duration
}

// ErrLoginURLUnavailable means no interactive login is pending.
var ErrLoginURLUnavailable = errors.New("tailscale login URL is not available")

// ErrInvalidLoginURL means control returned something that is not an https URL.
var ErrInvalidLoginURL = errors.New("tailscale login URL is invalid")

// Status never starts tsnet.
func (c *Controller) Status(ctx context.Context) Status {
	life := c.Lifecycle.Snapshot()
	switch life.Phase {
	case PhaseClosed:
		return Status{Name: c.Name, State: StateStopped, TailnetIPs: []string{}}
	case PhaseIdle:
		return Status{Name: c.Name, State: StateIdle, TailnetIPs: []string{}}
	}
	if !life.ServerStarted {
		if life.Phase == PhaseFailed {
			return ErrorStatus(c.Name, life.Err)
		}
		return Status{Name: c.Name, State: StateStarting, TailnetIPs: []string{}}
	}
	snapshot, err := c.Backend.Query(ctx)
	if err != nil {
		return ErrorStatus(c.Name, err)
	}
	status := FromSnapshot(c.Name, snapshot)
	if life.Phase == PhaseFailed && status.State != StateRunning && status.State != StateNeedsLogin {
		failed := ErrorStatus(c.Name, life.Err)
		failed.TailnetIPs, failed.Hostname = status.TailnetIPs, status.Hostname
		return failed
	}
	return status
}

// Login starts tsnet if needed and requests an interactive login. It returns
// immediately; clients poll Status until LoginURLAvailable or Running.
func (c *Controller) Login(ctx context.Context) error {
	if c.Lifecycle.Snapshot().Phase == PhaseClosed {
		return ErrClosed
	}
	background := c.Background
	if background == nil {
		background = context.Background()
	}
	timeout := c.LoginTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	go func() {
		ctx, cancel := context.WithTimeout(background, timeout)
		defer cancel()
		_ = c.login(ctx)
	}()
	return nil
}

func (c *Controller) login(ctx context.Context) error {
	if err := c.Backend.Start(); err != nil {
		return err
	}
	for {
		if !c.Lifecycle.Snapshot().ServerStarted {
			return ErrNotStarted
		}
		snapshot, err := c.Backend.Query(ctx)
		if err != nil {
			return err
		}
		switch snapshot.BackendState {
		case "Running", "NeedsMachineAuth":
			return nil
		case "NeedsLogin", "Stopped":
			if validLoginURL(snapshot.AuthURL) == nil {
				return nil
			}
			return c.Backend.LoginInteractive(ctx)
		}
		// NoState/Starting: wait for the backend to settle.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// LoginURL returns the pending interactive login URL. Callers must keep it in
// memory only, never log or persist it. It never starts tsnet.
func (c *Controller) LoginURL(ctx context.Context) (string, error) {
	if err := c.requireStarted(); err != nil {
		return "", err
	}
	snapshot, err := c.Backend.Query(ctx)
	if err != nil {
		return "", errors.New(Redact(err.Error()))
	}
	if snapshot.AuthURL == "" || (snapshot.BackendState != "NeedsLogin" && snapshot.BackendState != "Stopped") {
		return "", ErrLoginURLUnavailable
	}
	if err := validLoginURL(snapshot.AuthURL); err != nil {
		return "", err
	}
	return snapshot.AuthURL, nil
}

// Logout deauthorizes this node. The next Login creates a fresh login request.
func (c *Controller) Logout(ctx context.Context) error {
	if err := c.requireStarted(); err != nil {
		return err
	}
	if err := c.Backend.Logout(ctx); err != nil {
		return errors.New(Redact(err.Error()))
	}
	return nil
}

func (c *Controller) requireStarted() error {
	life := c.Lifecycle.Snapshot()
	if life.Phase == PhaseClosed {
		return ErrClosed
	}
	if !life.ServerStarted {
		return ErrNotStarted
	}
	return nil
}

func validLoginURL(raw string) error {
	if raw == "" {
		return ErrLoginURLUnavailable
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil {
		return ErrInvalidLoginURL
	}
	return nil
}

// Provider is implemented by Mihomo tailscale outbounds through the overlay.
type Provider interface {
	TailscaleController() *Controller
}
