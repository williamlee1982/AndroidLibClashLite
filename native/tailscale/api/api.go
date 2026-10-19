// Package api exposes typed Tailscale outbound state through the
// authenticated Clash API. Import it for its registration side effect.
//
//	GET  /asterisk/tailscale                     all tailscale outbounds
//	GET  /asterisk/tailscale/status?name=<name>  one outbound
//	POST /asterisk/tailscale/login?name=<name>   start tsnet and request login
//	GET  /asterisk/tailscale/login-url?name=...  pending login URL (secret required)
//	POST /asterisk/tailscale/logout?name=<name>  deauthorize this node
//
// Status never carries a login URL. login-url is refused on listeners without
// a controller secret and is sent with Cache-Control: no-store.
package api

import (
	"context"
	"errors"
	"time"

	"cfa/native/tailscale"

	"github.com/metacubex/mihomo/hub/route"

	"github.com/metacubex/chi"
	"github.com/metacubex/chi/render"
	"github.com/metacubex/http"
)

const Path = "/asterisk/tailscale"

const queryTimeout = 8 * time.Second

func init() {
	route.Register(func(r chi.Router) {
		r.Get(Path, listStatus)
		r.Get(Path+"/status", getStatus)
		r.Post(Path+"/login", login)
		r.Get(Path+"/login-url", loginURL)
		r.Post(Path+"/logout", logout)
	})
}

type listResponse struct {
	Outbounds []tailscale.Status `json:"outbounds"`
}

type loginURLResponse struct {
	URL string `json:"url"`
}

func listStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	render.JSON(w, r, listResponse{Outbounds: Statuses(ctx)})
}

func getStatus(w http.ResponseWriter, r *http.Request) {
	controller, ok := lookup(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	render.JSON(w, r, controller.Status(ctx))
}

func login(w http.ResponseWriter, r *http.Request) {
	controller, ok := lookup(w, r)
	if !ok {
		return
	}
	if err := controller.Login(r.Context()); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func loginURL(w http.ResponseWriter, r *http.Request) {
	if !tailscale.ControllerAuthenticated(r.Context()) {
		render.Status(r, http.StatusForbidden)
		render.JSON(w, r, &route.HTTPError{Message: "controller secret required"})
		return
	}
	controller, ok := lookup(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	url, err := controller.LoginURL(ctx)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	render.JSON(w, r, loginURLResponse{URL: url})
}

func logout(w http.ResponseWriter, r *http.Request) {
	controller, ok := lookup(w, r)
	if !ok {
		return
	}
	if err := controller.Logout(r.Context()); err != nil {
		writeError(w, r, err)
		return
	}
	render.NoContent(w, r)
}

func lookup(w http.ResponseWriter, r *http.Request) (*tailscale.Controller, bool) {
	name := r.URL.Query().Get("name")
	if name == "" {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, route.ErrBadRequest)
		return nil, false
	}
	controller, err := Lookup(name)
	if err != nil {
		writeError(w, r, err)
		return nil, false
	}
	return controller, true
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, tailscale.ErrNotStarted), errors.Is(err, tailscale.ErrClosed),
		errors.Is(err, tailscale.ErrLoginURLUnavailable):
		status = http.StatusConflict
	case errors.Is(err, tailscale.ErrInvalidLoginURL):
		status = http.StatusBadGateway
	}
	render.Status(r, status)
	render.JSON(w, r, &route.HTTPError{Message: tailscale.Redact(err.Error())})
}
