package server

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"smtpfilter/internal/config"
)

// maxCommandLine is the command line limit of RFC 5321, in octets,
// including the terminating carriage return and line feed.
const maxCommandLine = 512

// maxDataLine bounds one line within the DATA stream. RFC 5321 permits
// 1000 octets. A larger value is tolerated so that a sender with long
// header lines is not refused, but the value is bounded so that a single
// line cannot consume unlimited memory.
const maxDataLine = 65536

// errLineTooLong reports a command line that exceeds the protocol limit.
var errLineTooLong = errors.New("line too long")

// session holds the state of one connection. It is owned by one goroutine
// and is not shared.
type session struct {
	srv  *Server
	cfg  *config.Config
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	log  *slog.Logger

	mode      config.TLSMode
	encrypted bool
	peerIP    net.IP

	deadline time.Time

	// Transaction state. Cleared by RSET, by STARTTLS, and after the dot.
	helo  string
	from  string
	rcpt  string
	route *config.Route
	inTx  bool
}

// handleConn runs one session from the banner to the close.
func (s *Server) handleConn(conn net.Conn, mode config.TLSMode) {
	defer conn.Close()

	cfg := s.cfg.Load()

	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	ip := net.ParseIP(host)

	ss := &session{
		srv:      s,
		cfg:      cfg,
		conn:     conn,
		r:        bufio.NewReaderSize(conn, 4096),
		w:        bufio.NewWriterSize(conn, 4096),
		mode:     mode,
		peerIP:   ip,
		deadline: time.Now().Add(cfg.Limits.SessionTimeout()),
	}
	ss.encrypted = mode == config.TLSImplicit
	ss.log = s.log.With("peer", host, "tls", ss.encrypted)

	ss.reply(220, cfg.Hostname+" ready")
	ss.loop()
}

// loop reads and dispatches commands until the connection ends.
func (s *session) loop() {
	for {
		line, err := s.readCommand()
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				s.reply(500, "Line too long")
			}
			return
		}

		verb, arg := splitCommand(line)
		switch verb {
		case "EHLO":
			s.handleEHLO(arg)
		case "HELO":
			s.handleHELO(arg)
		case "STARTTLS":
			if !s.handleSTARTTLS(arg) {
				return
			}
		case "MAIL":
			s.handleMail(arg)
			if !s.inTx && s.from == "" {
				// The admission decision refused the sender. The reply has
				// already been written and the connection now closes.
				return
			}
		case "RCPT":
			if !s.handleRcpt(arg) {
				return
			}
		case "DATA":
			if !s.handleData(arg) {
				return
			}
		case "RSET":
			s.resetTx()
			s.reply(250, "OK")
		case "NOOP":
			s.reply(250, "OK")
		case "QUIT":
			s.reply(221, s.cfg.Hostname+" closing connection")
			return
		case "AUTH", "VRFY", "EXPN", "ETRN", "HELP":
			s.reply(502, "Command not implemented")
		case "":
			s.reply(500, "Syntax error")
		default:
			s.reply(500, "Syntax error, command unrecognised")
		}
	}
}

// handleEHLO returns the hostname and the extension list. Only implemented
// extensions are advertised.
func (s *session) handleEHLO(arg string) {
	if arg == "" {
		s.reply(501, "EHLO requires a domain")
		return
	}
	s.helo = arg
	s.resetTx()

	lines := []string{
		s.cfg.Hostname + " greets " + arg,
		"SIZE " + itoa(s.cfg.Limits.MaxMessageBytes),
		"8BITMIME",
		"PIPELINING",
	}
	if s.mode == config.TLSStartTLS && !s.encrypted && s.srv.tlsConf != nil {
		lines = append(lines, "STARTTLS")
	}
	s.replyMulti(250, lines)
}

// handleHELO returns the hostname only.
func (s *session) handleHELO(arg string) {
	if arg == "" {
		s.reply(501, "HELO requires a domain")
		return
	}
	s.helo = arg
	s.resetTx()
	s.reply(250, s.cfg.Hostname)
}

// handleSTARTTLS upgrades the connection. It returns false when the session
// must end.
//
// After a successful handshake the session state is reset to the state that
// exists immediately after connection establishment. The client must send
// EHLO again and any earlier reverse-path is discarded. RFC 3207 requires
// this reset. It is a correctness requirement and not an option.
func (s *session) handleSTARTTLS(arg string) bool {
	if arg != "" {
		s.reply(501, "STARTTLS takes no argument")
		return true
	}
	if s.mode != config.TLSStartTLS || s.srv.tlsConf == nil {
		s.reply(502, "Command not implemented")
		return true
	}
	if s.encrypted {
		s.reply(503, "Already encrypted")
		return true
	}

	s.reply(220, "Ready to start TLS")

	s.conn.SetDeadline(time.Now().Add(s.cfg.Limits.CommandTimeout()))
	tc := tls.Server(s.conn, s.srv.tlsConf)
	if err := tc.Handshake(); err != nil {
		s.log.Warn("tls handshake failed", "err", err)
		return false
	}

	s.conn = tc
	s.r = bufio.NewReaderSize(tc, 4096)
	s.w = bufio.NewWriterSize(tc, 4096)
	s.encrypted = true
	s.helo = ""
	s.resetTx()
	s.log = s.srv.log.With("peer", s.peerIP.String(), "tls", true)
	return true
}

// resetTx clears the transaction state. The body buffer is not held on the
// session, so nothing else needs releasing here.
func (s *session) resetTx() {
	s.from = ""
	s.rcpt = ""
	s.route = nil
	s.inTx = false
}

// readCommand reads one command line and enforces the protocol limit. A
// bare line feed is rejected in the command stream.
func (s *session) readCommand() (string, error) {
	if err := s.setReadDeadline(); err != nil {
		return "", err
	}
	line, err := readLine(s.r, maxCommandLine)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(line, "\r\n") {
		return "", errors.New("bare line feed in command stream")
	}
	return strings.TrimSuffix(line, "\r\n"), nil
}

// setReadDeadline applies the command timeout, bounded by the remaining
// session lifetime.
func (s *session) setReadDeadline() error {
	d := time.Now().Add(s.cfg.Limits.CommandTimeout())
	if d.After(s.deadline) {
		d = s.deadline
	}
	if !time.Now().Before(d) {
		return errors.New("session timeout")
	}
	return s.conn.SetReadDeadline(d)
}

// readLine reads one line including its terminator, up to max octets. A
// longer line yields errLineTooLong and the remainder is not consumed,
// because the connection is closed in that case.
func readLine(r *bufio.Reader, max int) (string, error) {
	var sb strings.Builder
	for {
		chunk, err := r.ReadString('\n')
		sb.WriteString(chunk)
		if sb.Len() > max {
			return "", errLineTooLong
		}
		if err != nil {
			if errors.Is(err, io.EOF) && sb.Len() > 0 {
				return sb.String(), io.EOF
			}
			return "", err
		}
		return sb.String(), nil
	}
}

// reply writes one single-line reply.
func (s *session) reply(code int, text string) {
	s.conn.SetWriteDeadline(time.Now().Add(s.cfg.Limits.CommandTimeout()))
	s.w.WriteString(itoa(int64(code)))
	s.w.WriteByte(' ')
	s.w.WriteString(text)
	s.w.WriteString("\r\n")
	s.w.Flush()
}

// replyMulti writes one multi-line reply.
func (s *session) replyMulti(code int, lines []string) {
	s.conn.SetWriteDeadline(time.Now().Add(s.cfg.Limits.CommandTimeout()))
	c := itoa(int64(code))
	for i, l := range lines {
		s.w.WriteString(c)
		if i == len(lines)-1 {
			s.w.WriteByte(' ')
		} else {
			s.w.WriteByte('-')
		}
		s.w.WriteString(l)
		s.w.WriteString("\r\n")
	}
	s.w.Flush()
}

// splitCommand separates the verb from its argument. The verb is upper
// cased. The argument keeps its original case, because an address is
// compared in lower case only after it is parsed.
func splitCommand(line string) (verb, arg string) {
	line = strings.TrimLeft(line, " ")
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return strings.ToUpper(line), ""
	}
	return strings.ToUpper(line[:i]), strings.TrimSpace(line[i+1:])
}

// itoa formats an integer without importing strconv into every file.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

