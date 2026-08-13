// Package server implements the SMTP listener and the session state
// machine. One goroutine runs per listener and one per connection.
package server

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"smtpfilter/internal/config"
	"smtpfilter/internal/msg"
	"smtpfilter/internal/policy"
)

// Server holds the state shared by every session.
type Server struct {
	cfg        atomic.Pointer[config.Config]
	resolver   policy.Resolver
	dispatcher msg.Dispatcher
	tlsConf    *tls.Config
	sem        chan struct{}
	log        *slog.Logger

	mu        sync.Mutex
	listeners []net.Listener
	closing   bool
}

// New returns a server. The configuration is held behind an atomic pointer
// so that it can be replaced without stopping the listeners. A session
// reads the pointer once at start and uses that value for its whole
// lifetime.
func New(cfg *config.Config, r policy.Resolver, d msg.Dispatcher,
	tlsConf *tls.Config, log *slog.Logger) *Server {

	s := &Server{
		resolver:   r,
		dispatcher: d,
		tlsConf:    tlsConf,
		sem:        make(chan struct{}, cfg.Limits.MaxConnections),
		log:        log,
	}
	s.cfg.Store(cfg)
	return s
}

// Reload replaces the configuration. Sessions already in progress continue
// with the configuration they began with.
func (s *Server) Reload(cfg *config.Config) {
	s.cfg.Store(cfg)
}

// ListenAndServe binds every configured listener and serves until Close is
// called. It returns when every listener has stopped.
func (s *Server) ListenAndServe() error {
	cfg := s.cfg.Load()

	var wg sync.WaitGroup
	var errs []error
	var errMu sync.Mutex

	for _, l := range cfg.Listeners {
		ln, err := s.bind(l)
		if err != nil {
			s.Close()
			wg.Wait()
			return err
		}
		s.mu.Lock()
		s.listeners = append(s.listeners, ln)
		s.mu.Unlock()

		wg.Add(1)
		go func(ln net.Listener, mode config.TLSMode) {
			defer wg.Done()
			if err := s.serve(ln, mode); err != nil {
				errMu.Lock()
				errs = append(errs, err)
				errMu.Unlock()
			}
		}(ln, l.TLS)
	}

	wg.Wait()
	return errors.Join(errs...)
}

// bind creates one listener. An implicit mode listener is wrapped in TLS at
// the listener level, so that the connection is encrypted from the first
// byte.
func (s *Server) bind(l config.Listener) (net.Listener, error) {
	ln, err := net.Listen("tcp", l.Addr)
	if err != nil {
		return nil, err
	}
	if l.TLS == config.TLSImplicit {
		if s.tlsConf == nil {
			ln.Close()
			return nil, errors.New("implicit tls listener without a tls config")
		}
		ln = tls.NewListener(ln, s.tlsConf)
	}
	s.log.Info("listening", "addr", l.Addr, "tls", string(l.TLS))
	return ln, nil
}

// serve is the accept loop. Go supplies no accept loop for raw TCP, so it
// is written here. The semaphore bounds the number of concurrent sessions.
func (s *Server) serve(ln net.Listener, mode config.TLSMode) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}

		select {
		case s.sem <- struct{}{}:
		default:
			// The connection limit is reached. Close without a reply.
			s.log.Warn("connection limit reached",
				"peer", conn.RemoteAddr().String())
			conn.Close()
			continue
		}

		go func(c net.Conn) {
			defer func() { <-s.sem }()
			s.handleConn(c, mode)
		}(conn)
	}
}

// Close stops every listener. Sessions in progress are not interrupted.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
	for _, ln := range s.listeners {
		ln.Close()
	}
	s.listeners = nil
}
