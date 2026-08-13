package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"smtpfilter/internal/msg"
	"smtpfilter/internal/policy"
)

// handleMail parses the reverse-path and applies the admission decision.
//
// Rejection before the dot is permitted and correct. It discloses nothing
// the sender does not already possess, and it prevents the transmission of
// a body that would be discarded.
func (s *session) handleMail(arg string) {
	if s.helo == "" {
		s.reply(503, "Send EHLO first")
		return
	}
	if s.inTx {
		s.reply(503, "Nested MAIL command")
		return
	}

	path, ok := parsePath(arg, "FROM:")
	if !ok {
		s.reply(501, "Syntax error in MAIL parameters")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		s.cfg.Limits.CommandTimeout())
	defer cancel()

	d := policy.Decide(ctx, s.srv.resolver, s.cfg, s.peerIP, path)

	switch d.Outcome {
	case policy.Admit:
		s.from = path
		s.inTx = true
		s.log.Info("sender admitted", "from", path, "rule", d.Rule,
			"spf", string(d.SPF), "ptr", d.PTRName)
		s.reply(250, "OK")

	case policy.RejectTemporary:
		// A DNS failure is not evidence of forgery, so the sender is asked
		// to retry rather than having its message discarded.
		s.log.Warn("sender deferred", "from", path, "rule", d.Rule,
			"spf", string(d.SPF), "reason", d.Reason)
		s.reply(451, "Temporary failure, try again later")

	default:
		s.log.Warn("sender rejected", "from", path, "rule", d.Rule,
			"spf", string(d.SPF), "reason", d.Reason)
		s.reply(550, "Sender not permitted")
	}
}

// handleRcpt looks the forward-path up in the route table. It returns false
// when the session must end.
//
// The forward-path is not a mailbox. It is a key into the route table.
// Because there is no catch-all route, an attempt to enumerate addresses
// produces no result.
func (s *session) handleRcpt(arg string) bool {
	if !s.inTx {
		s.reply(503, "Send MAIL first")
		return true
	}
	if s.rcpt != "" {
		// The disposition is selected by the recipient. A message with two
		// recipients would select two dispositions with no defined
		// ordering or failure semantics.
		s.reply(452, "Too many recipients")
		return true
	}

	path, ok := parsePath(arg, "TO:")
	if !ok {
		s.reply(501, "Syntax error in RCPT parameters")
		return true
	}

	route := s.cfg.LookupRoute(path)
	if route == nil {
		s.log.Warn("recipient rejected", "from", s.from, "to", path)
		s.reply(550, "Recipient not permitted")
		return false
	}

	s.rcpt = path
	s.route = route
	s.reply(250, "OK")
	return true
}

// handleData reads the body and reaches the commit point. It returns false
// when the session must end.
func (s *session) handleData(arg string) bool {
	if arg != "" {
		s.reply(501, "DATA takes no argument")
		return true
	}
	if !s.inTx || s.rcpt == "" {
		s.reply(503, "Send MAIL and RCPT first")
		return true
	}

	s.reply(354, "Send data, end with a single dot")

	body, oversize, err := s.readBody(s.cfg.Limits.MaxMessageBytes)
	if err != nil {
		return false
	}

	// Everything below this line is past the commit point. The reply is
	// always 250. No failure of any downstream system is reported to the
	// sending host, and the sender therefore never retries.
	if oversize {
		s.log.Error("message discarded, body exceeded size limit",
			"from", s.from, "to", s.rcpt, "route", s.route.Recipient)
		s.finish()
		return true
	}

	m := &msg.Message{
		From:     s.from,
		To:       s.rcpt,
		Route:    s.route,
		Body:     body,
		PeerIP:   s.peerIP,
		Received: time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.route.Timeout())
	res, derr := s.srv.dispatcher.Dispatch(ctx, m)
	cancel()

	switch res {
	case msg.ResultOK:
		s.log.Info("message delivered", "from", s.from, "to", s.rcpt,
			"route", s.route.Recipient, "bytes", len(body))
	case msg.ResultQueued:
		s.log.Warn("message queued for retry", "from", s.from, "to", s.rcpt,
			"route", s.route.Recipient, "bytes", len(body), "err", derr)
	case msg.ResultTempFail:
		s.log.Error("message discarded, temporary failure and no queue "+
			"capacity", "from", s.from, "to", s.rcpt,
			"route", s.route.Recipient, "err", derr)
	default:
		s.log.Error("message discarded, permanent failure",
			"from", s.from, "to", s.rcpt, "route", s.route.Recipient,
			"err", derr)
	}

	s.finish()
	return true
}

// finish clears the transaction and returns the only reply permitted after
// the dot.
func (s *session) finish() {
	s.resetTx()
	s.reply(250, "OK")
}

// readBody reads the DATA stream to the terminating dot. It reverses the
// dot-stuffing applied by the sender and normalises a bare line feed to a
// carriage return and line feed.
//
// When the size limit is exceeded the buffer is released and the remainder
// of the stream is read and discarded, so that the connection stays in a
// defined state and the caller can return 250. A 552 reply is not sent,
// because the message has passed the commit boundary.
func (s *session) readBody(max int64) (body []byte, oversize bool, err error) {
	buf := make([]byte, 0, 8192)
	var total int64

	for {
		if err := s.setReadDeadline(); err != nil {
			return nil, false, err
		}
		line, rerr := readLine(s.r, maxDataLine)
		if rerr != nil {
			if errors.Is(rerr, errLineTooLong) {
				// Discard the remainder of this line, then continue in the
				// oversize state.
				if derr := discardLine(s.r); derr != nil {
					return nil, false, derr
				}
				oversize = true
				buf = nil
				continue
			}
			if errors.Is(rerr, io.EOF) {
				return nil, false, io.ErrUnexpectedEOF
			}
			return nil, false, rerr
		}

		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			if oversize {
				return nil, true, nil
			}
			return buf, false, nil
		}

		// Reverse the dot-stuffing applied by the sender.
		if strings.HasPrefix(trimmed, "..") {
			trimmed = trimmed[1:]
		}

		total += int64(len(trimmed)) + 2
		if total > max {
			oversize = true
			buf = nil
			continue
		}
		if oversize {
			continue
		}

		buf = append(buf, trimmed...)
		buf = append(buf, '\r', '\n')
	}
}

// discardLine consumes the remainder of an over-length line.
func discardLine(r *bufio.Reader) error {
	for {
		_, err := r.ReadString('\n')
		if err == nil {
			return nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return err
	}
}

// parsePath extracts an address from a MAIL or RCPT argument. The address
// may be enclosed in angle brackets. Parameters after the address, such as
// SIZE, are ignored.
func parsePath(arg, prefix string) (string, bool) {
	if len(arg) < len(prefix) ||
		!strings.EqualFold(arg[:len(prefix)], prefix) {
		return "", false
	}
	rest := strings.TrimSpace(arg[len(prefix):])
	if rest == "" {
		return "", false
	}

	if strings.HasPrefix(rest, "<") {
		end := strings.Index(rest, ">")
		if end < 0 {
			return "", false
		}
		return strings.ToLower(strings.TrimSpace(rest[1:end])), true
	}

	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		rest = rest[:i]
	}
	return strings.ToLower(rest), true
}
