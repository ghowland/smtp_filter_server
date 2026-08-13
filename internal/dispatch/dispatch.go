// Package dispatch performs the disposition selected by the route. Three
// dispositions exist: execution of a local program, transmission of an HTTP
// request, and relay to a downstream mail server.
package dispatch

import (
	"context"
	"fmt"
	"log/slog"

	"smtpfilter/internal/config"
	"smtpfilter/internal/msg"
)

// Router selects the disposition implementation from the route type.
type Router struct {
	cmd  *Command
	hook *Webhook
	fwd  *Forward
	log  *slog.Logger
}

// NewRouter returns a router holding one instance of each disposition.
func NewRouter(log *slog.Logger) *Router {
	return &Router{
		cmd:  NewCommand(log),
		hook: NewWebhook(log),
		fwd:  NewForward(log),
		log:  log,
	}
}

// Dispatch performs one disposition attempt. The context carries the route
// timeout and is set by the caller.
func (r *Router) Dispatch(ctx context.Context, m *msg.Message) (msg.Result, error) {
	if m.Route == nil {
		return msg.ResultPermFail, fmt.Errorf("message has no route")
	}
	switch m.Route.Type {
	case config.RouteCommand:
		return r.cmd.Dispatch(ctx, m)
	case config.RouteWebhook:
		return r.hook.Dispatch(ctx, m)
	case config.RouteForward:
		return r.fwd.Dispatch(ctx, m)
	default:
		return msg.ResultPermFail,
			fmt.Errorf("unknown route type %q", m.Route.Type)
	}
}

