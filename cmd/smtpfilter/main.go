// Command smtpfilter is a minimal SMTP server that admits mail only from a
// configured set of senders and performs a configured action on each
// admitted message.
//
// The process performs no disk write operations. Message data exists only
// in memory and does not survive a restart.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"smtpfilter/internal/config"
	"smtpfilter/internal/dispatch"
	"smtpfilter/internal/msg"
	"smtpfilter/internal/policy"
	"smtpfilter/internal/queue"
	"smtpfilter/internal/server"
)

// drainLimit bounds the final delivery attempt at shutdown.
const drainLimit = 30 * time.Second

func main() {
	var (
		path  = flag.String("config", "config.json", "configuration file")
		check = flag.Bool("check", false, "validate the configuration and exit")
		debug = flag.Bool("debug", false, "log at debug level")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout,
		&slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *check {
		fmt.Println("configuration is valid")
		return
	}

	var tlsConf *tls.Config
	if cfg.TLS.Cert != "" && cfg.TLS.Key != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load certificate:", err)
			os.Exit(2)
		}
		tlsConf = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	resolver := policy.NewNetResolver(cfg.DNS.Timeout(), cfg.DNS.Server)

	// The queue wraps the router, so that the session sees one dispatcher
	// and the retry behaviour is invisible to it.
	router := dispatch.NewRouter(log)
	var dispatcher msg.Dispatcher = router
	var q *queue.Queue
	if cfg.Retry.Enabled {
		q = queue.New(router, cfg.Retry, log)
		q.Start()
		dispatcher = q
	}

	srv := server.New(cfg, resolver, dispatcher, tlsConf, log)

	// Signals. INT and TERM stop the server. HUP reloads the configuration.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	log.Info("started", "hostname", cfg.Hostname,
		"listeners", len(cfg.Listeners), "routes", len(cfg.Routes),
		"retry", cfg.Retry.Enabled)

	for {
		select {
		case s := <-sig:
			if s == syscall.SIGHUP {
				// A reload replaces the configuration only. The listener
				// set and the retry parameters are fixed for the lifetime
				// of the process, because a session in progress holds the
				// configuration it began with.
				newCfg, err := config.Load(*path)
				if err != nil {
					log.Error("reload failed, keeping the current "+
						"configuration", "err", err)
					continue
				}
				srv.Reload(newCfg)
				log.Info("configuration reloaded",
					"routes", len(newCfg.Routes))
				continue
			}
			log.Info("shutting down", "signal", s.String())
			srv.Close()
			if q != nil {
				log.Info("draining queue", "entries", q.Len())
				q.Drain(drainLimit)
			}
			return

		case err := <-serveErr:
			if err != nil {
				log.Error("listener stopped", "err", err)
				if q != nil {
					q.Drain(drainLimit)
				}
				os.Exit(1)
			}
			return
		}
	}
}
