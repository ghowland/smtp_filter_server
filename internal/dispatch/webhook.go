package dispatch

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"smtpfilter/internal/msg"
)

// Webhook transmits the message body as the body of an HTTP POST request.
type Webhook struct {
	client *http.Client
	log    *slog.Logger
}

// NewWebhook returns a webhook disposition. Redirects are disabled, so that
// a compromised or misconfigured endpoint cannot direct the message body to
// a different host.
func NewWebhook(log *slog.Logger) *Webhook {
	return &Webhook{
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		log: log,
	}
}

// Dispatch posts the message.
//
// The URL is taken from configuration only and is never derived from the
// message content. The request body carries an HMAC signature computed with
// the per-route secret, so that the receiving system can authenticate the
// request.
func (w *Webhook) Dispatch(ctx context.Context, m *msg.Message) (msg.Result, error) {
	r := m.Route

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL,
		bytes.NewReader(m.Body))
	if err != nil {
		return msg.ResultPermFail, fmt.Errorf("build request: %w", err)
	}

	mac := hmac.New(sha256.New, []byte(r.Secret))
	mac.Write(m.Body)
	sig := hex.EncodeToString(mac.Sum(nil))

	req.Header.Set("Content-Type", "message/rfc822")
	req.Header.Set("X-Smtpfilter-From", m.From)
	req.Header.Set("X-Smtpfilter-To", m.To)
	req.Header.Set("X-Smtpfilter-Peer", m.PeerIP.String())
	req.Header.Set("X-Smtpfilter-Signature", "sha256="+sig)

	resp, err := w.client.Do(req)
	if err != nil {
		// A transport fault or a deadline expiry may not recur.
		return msg.ResultTempFail, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	// The response body is drained and discarded so that the connection can
	// return to the pool. The limit prevents an endpoint from returning an
	// unbounded body.
	io.CopyN(io.Discard, resp.Body, 4096)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return msg.ResultOK, nil
	case resp.StatusCode >= 500:
		return msg.ResultTempFail, fmt.Errorf("http status %d", resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return msg.ResultTempFail, fmt.Errorf("http status %d", resp.StatusCode)
	default:
		return msg.ResultPermFail, fmt.Errorf("http status %d", resp.StatusCode)
	}
}

