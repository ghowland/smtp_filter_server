// Package msg holds the message value that crosses the boundary between the
// SMTP session, the disposition implementations, and the retry queue, and
// the interface those implementations satisfy. It exists so that no two of
// those packages need to import one another.
package msg

import (
	"context"
	"net"
	"time"

	"smtpfilter/internal/config"
)

// Result is the outcome of one disposition attempt.
type Result int

const (
	// ResultOK means the message reached its destination.
	ResultOK Result = iota
	// ResultTempFail means the attempt may succeed if repeated.
	ResultTempFail
	// ResultPermFail means the attempt will not succeed if repeated.
	ResultPermFail
	// ResultQueued means the first attempt failed temporarily and the
	// message is held in memory for a later attempt.
	ResultQueued
)

// String returns the name of the result for the log.
func (r Result) String() string {
	switch r {
	case ResultOK:
		return "ok"
	case ResultTempFail:
		return "tempfail"
	case ResultPermFail:
		return "permfail"
	case ResultQueued:
		return "queued"
	}
	return "unknown"

}

// Message is one accepted message and the envelope that carried it.
type Message struct {
	From     string
	To       string
	Route    *config.Route
	Body     []byte
	PeerIP   net.IP
	Received time.Time
}

// Dispatcher performs the disposition selected by the route.
type Dispatcher interface {
	Dispatch(ctx context.Context, m *Message) (Result, error)
}

// StubDispatcher accepts every message and does nothing with it. It permits
// the server to be exercised before the real dispositions exist.
type StubDispatcher struct{}

// Dispatch discards the message and reports success.
func (StubDispatcher) Dispatch(context.Context, *Message) (Result, error) {
	return ResultOK, nil
}
