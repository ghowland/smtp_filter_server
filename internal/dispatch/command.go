package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"smtpfilter/internal/msg"
)

// maxCapture bounds the number of octets retained from the standard output
// and standard error of the child process. A program that writes without
// limit must not consume the memory of this process.
const maxCapture = 8192

// Command executes a local program and writes the message body to its
// standard input.
type Command struct {
	log *slog.Logger
}

// NewCommand returns a command disposition.
func NewCommand(log *slog.Logger) *Command {
	return &Command{log: log}
}

// Dispatch runs the configured program.
//
// The program is invoked directly. A shell is never used, so no part of the
// envelope can be interpreted as a shell construction. The envelope is
// supplied through environment variables and is never interpolated into a
// string that is later parsed.
func (c *Command) Dispatch(ctx context.Context, m *msg.Message) (msg.Result, error) {
	r := m.Route

	cmd := exec.CommandContext(ctx, r.Path, r.Args...)
	cmd.Stdin = bytes.NewReader(m.Body)

	var out, errOut boundedBuffer
	out.max = maxCapture
	errOut.max = maxCapture
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	cmd.Env = append(os.Environ(),
		"SMTPFILTER_FROM="+m.From,
		"SMTPFILTER_TO="+m.To,
		"SMTPFILTER_PEER="+m.PeerIP.String(),
		"SMTPFILTER_SIZE="+fmt.Sprint(len(m.Body)),
	)

	err := cmd.Run()

	if out.buf.Len() > 0 || errOut.buf.Len() > 0 {
		c.log.Info("command output", "route", r.Recipient,
			"stdout", out.buf.String(), "stderr", errOut.buf.String())
	}

	if err == nil {
		return msg.ResultOK, nil
	}

	// A context deadline is a temporary condition. The program may complete
	// within the limit on a later attempt.
	if ctx.Err() != nil {
		return msg.ResultTempFail, fmt.Errorf("command timeout: %w", err)
	}

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if r.TempFailExitCode != 0 && code == r.TempFailExitCode {
			return msg.ResultTempFail,
				fmt.Errorf("command exit %d, temporary", code)
		}
		return msg.ResultPermFail, fmt.Errorf("command exit %d", code)
	}

	// The program could not be started at all. A missing binary or a
	// permission fault will not resolve itself, but a fork failure under
	// memory pressure will, so this is treated as temporary.
	return msg.ResultTempFail, fmt.Errorf("command start: %w", err)
}

// boundedBuffer retains at most max octets and discards the remainder.
type boundedBuffer struct {
	buf bytes.Buffer
	max int
}

// Write retains what fits and reports the full length, so that the child
// process does not receive a short write and stop.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf.Write(p[:room])
	}
	return len(p), nil
}

