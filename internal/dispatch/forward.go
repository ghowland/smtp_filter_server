package dispatch

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"smtpfilter/internal/msg"
)

// Forward relays the message to a downstream mail server.
//
// The target is a mail server under the same administrative control. Sender
// Rewriting Scheme is therefore not applied, SPF alignment at the target is
// not a consideration, and outbound reputation is not managed. The original
// envelope is presented unchanged.
type Forward struct {
	log *slog.Logger
}

// NewForward returns a forward disposition.
func NewForward(log *slog.Logger) *Forward {
	return &Forward{log: log}
}

// Dispatch opens one SMTP session and transmits the message. The client is
// written directly rather than taken from net/smtp, because the deadline
// must apply to every read and write in the session.
func (f *Forward) Dispatch(ctx context.Context, m *msg.Message) (msg.Result, error) {
	r := m.Route
	addr := net.JoinHostPort(r.Host, strconv.Itoa(r.Port))

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return msg.ResultTempFail, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}

	c := &smtpConn{
		conn: conn,
		r:    bufio.NewReader(conn),
		w:    bufio.NewWriter(conn),
	}

	// The banner.
	if res, err := c.expect(220); err != nil {
		return res, err
	}

	if _, err := c.cmd("EHLO "+localName(conn), 250); err != nil {
		// A server that refuses EHLO may accept HELO.
		if res, err2 := c.cmd("HELO "+localName(conn), 250); err2 != nil {
			return res, err2
		}
	}

	if res, err := c.cmd("MAIL FROM:<"+m.From+">", 250); err != nil {
		return res, err
	}
	if res, err := c.cmd("RCPT TO:<"+m.To+">", 250); err != nil {
		return res, err
	}
	if res, err := c.cmd("DATA", 354); err != nil {
		return res, err
	}

	if err := c.writeBody(m.Body); err != nil {
		return msg.ResultTempFail, fmt.Errorf("write body: %w", err)
	}
	if res, err := c.expect(250); err != nil {
		return res, err
	}

	c.cmd("QUIT", 221)
	return msg.ResultOK, nil
}

// smtpConn is a minimal outbound SMTP client.
type smtpConn struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
}

// cmd writes one command and reads the reply.
func (c *smtpConn) cmd(line string, want int) (msg.Result, error) {
	if _, err := c.w.WriteString(line + "\r\n"); err != nil {
		return msg.ResultTempFail, err
	}
	if err := c.w.Flush(); err != nil {
		return msg.ResultTempFail, err
	}
	return c.expect(want)
}

// expect reads a reply, consuming every continuation line, and compares the
// code. A 4xx reply or a transport fault is temporary. A 5xx reply is
// permanent.
func (c *smtpConn) expect(want int) (msg.Result, error) {
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return msg.ResultTempFail, err
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 3 {
			return msg.ResultTempFail, fmt.Errorf("short reply %q", line)
		}
		code, err := strconv.Atoi(line[:3])
		if err != nil {
			return msg.ResultTempFail, fmt.Errorf("bad reply %q", line)
		}
		// A hyphen in the fourth position marks a continuation line.
		if len(line) > 3 && line[3] == '-' {
			continue
		}
		if code == want {
			return msg.ResultOK, nil
		}
		if code >= 500 {
			return msg.ResultPermFail, fmt.Errorf("target replied %q", line)
		}
		return msg.ResultTempFail, fmt.Errorf("target replied %q", line)
	}
}

// writeBody transmits the body with dot-stuffing applied and terminates it
// with the dot.
func (c *smtpConn) writeBody(body []byte) error {
	for _, line := range strings.Split(string(body), "\r\n") {
		if strings.HasPrefix(line, ".") {
			if _, err := c.w.WriteString("."); err != nil {
				return err
			}
		}
		if _, err := c.w.WriteString(line + "\r\n"); err != nil {
			return err
		}
	}
	if _, err := c.w.WriteString(".\r\n"); err != nil {
		return err
	}
	return c.w.Flush()
}

// localName returns the name this process presents in EHLO to the target.
func localName(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil || host == "" {
		return "localhost"
	}
	return "[" + host + "]"
}
