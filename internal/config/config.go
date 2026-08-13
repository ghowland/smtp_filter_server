// Package config defines the JSON configuration structures for the SMTP
// filter server, validates them at load time, and precomputes the derived
// values that the connection path must not recalculate.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// TLSMode selects the transport security behaviour of one listener.
type TLSMode string

const (
	// TLSPlain disables encryption. STARTTLS is not advertised.
	TLSPlain TLSMode = "plain"
	// TLSStartTLS begins unencrypted and advertises STARTTLS.
	TLSStartTLS TLSMode = "starttls"
	// TLSImplicit encrypts the connection from the first byte.
	TLSImplicit TLSMode = "implicit"
)

// UnmarshalJSON rejects any value that is not one of the three defined modes.
func (m *TLSMode) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch TLSMode(s) {
	case TLSPlain, TLSStartTLS, TLSImplicit:
		*m = TLSMode(s)
		return nil
	default:
		return fmt.Errorf("unknown tls mode %q", s)
	}
}

// MatchType selects how a whitelist entry is compared to a reverse-path.
type MatchType string

const (
	// MatchAddress compares the complete address.
	MatchAddress MatchType = "address"
	// MatchDomain compares the domain part only.
	MatchDomain MatchType = "domain"
)

// UnmarshalJSON rejects any value that is not one of the two defined types.
func (m *MatchType) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch MatchType(s) {
	case MatchAddress, MatchDomain:
		*m = MatchType(s)
		return nil
	default:
		return fmt.Errorf("unknown match type %q", s)
	}
}

// RouteType selects the disposition performed on an accepted message.
type RouteType string

const (
	// RouteCommand executes a local program.
	RouteCommand RouteType = "command"
	// RouteWebhook transmits an HTTP request.
	RouteWebhook RouteType = "webhook"
	// RouteForward relays the message to a downstream mail server.
	RouteForward RouteType = "forward"
)

// UnmarshalJSON rejects any value that is not one of the three defined types.
func (t *RouteType) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch RouteType(s) {
	case RouteCommand, RouteWebhook, RouteForward:
		*t = RouteType(s)
		return nil
	default:
		return fmt.Errorf("unknown route type %q", s)
	}
}

// Listener describes one bound address and its transport security mode.
type Listener struct {
	Addr string  `json:"addr"`
	TLS  TLSMode `json:"tls"`
}

// TLS holds the paths to the server certificate and private key.
type TLS struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// Limits bounds memory use and connection lifetime.
type Limits struct {
	MaxMessageBytes   int64 `json:"max_message_bytes"`
	MaxConnections    int   `json:"max_connections"`
	CommandTimeoutSec int   `json:"command_timeout_sec"`
	SessionTimeoutSec int   `json:"session_timeout_sec"`
}

// CommandTimeout is the permitted interval between individual commands.
func (l Limits) CommandTimeout() time.Duration {
	return time.Duration(l.CommandTimeoutSec) * time.Second
}

// SessionTimeout is the total permitted lifetime of one connection.
func (l Limits) SessionTimeout() time.Duration {
	return time.Duration(l.SessionTimeoutSec) * time.Second
}

// DNS configures the resolver used by the admission rules.
type DNS struct {
	TimeoutSec int    `json:"timeout_sec"`
	Server     string `json:"server"`
}

// Timeout is the deadline applied to every DNS query.
func (d DNS) Timeout() time.Duration {
	return time.Duration(d.TimeoutSec) * time.Second
}

// Retry configures the in-memory queue that holds messages whose first
// disposition attempt failed temporarily.
type Retry struct {
	Enabled     bool  `json:"enabled"`
	IntervalSec int   `json:"interval_sec"`
	ExpireSec   int   `json:"expire_sec"`
	MaxEntries  int   `json:"max_entries"`
	MaxBytes    int64 `json:"max_bytes"`
}

// Interval is the period of the queue worker ticker.
func (r Retry) Interval() time.Duration {
	return time.Duration(r.IntervalSec) * time.Second
}

// Expire is the age at which a queue entry is discarded.
func (r Retry) Expire() time.Duration {
	return time.Duration(r.ExpireSec) * time.Second
}

// CIDREntry admits a peer by address alone. No DNS query is performed.
type CIDREntry struct {
	CIDR    string   `json:"cidr"`
	Domains []string `json:"domains"`

	// Net is the parsed form of CIDR, computed at load time.
	Net *net.IPNet `json:"-"`
}

// Provider admits a peer whose forward-confirmed reverse DNS name falls
// under one of the listed suffixes, for the listed sender domains only.
type Provider struct {
	Name        string   `json:"name"`
	PTRSuffixes []string `json:"ptr_suffixes"`
	Domains     []string `json:"domains"`
	RequireSPF  *bool    `json:"require_spf"`
}

// SPFRequired reports whether SPF must be evaluated for this provider.
// The default when the field is absent is true.
func (p Provider) SPFRequired() bool {
	if p.RequireSPF == nil {
		return true
	}
	return *p.RequireSPF
}

// WhitelistEntry admits a peer by the reverse-path it presents.
type WhitelistEntry struct {
	Match      MatchType `json:"match"`
	Value      string    `json:"value"`
	RequireSPF *bool     `json:"require_spf"`
}

// SPFRequired reports whether SPF must be evaluated for this entry.
// The default when the field is absent is true.
func (w WhitelistEntry) SPFRequired() bool {
	if w.RequireSPF == nil {
		return true
	}
	return *w.RequireSPF
}

// Route maps a forward-path to a disposition.
type Route struct {
	Recipient string    `json:"recipient"`
	Match     MatchType `json:"match"`
	Type      RouteType `json:"type"`

	// Command disposition.
	Path             string   `json:"path"`
	Args             []string `json:"args"`
	TempFailExitCode int      `json:"temp_fail_exit_code"`

	// Webhook disposition.
	URL    string `json:"url"`
	Secret string `json:"secret"`

	// Forward disposition.
	Host string `json:"host"`
	Port int    `json:"port"`

	TimeoutSec int `json:"timeout_sec"`
}

// Timeout is the deadline applied to one disposition attempt.
func (r Route) Timeout() time.Duration {
	return time.Duration(r.TimeoutSec) * time.Second
}

// Config is the complete server configuration. It is immutable after Load
// returns and is safe for concurrent read by any number of goroutines.
type Config struct {
	Hostname      string           `json:"hostname"`
	Listeners     []Listener       `json:"listeners"`
	TLS           TLS              `json:"tls"`
	Limits        Limits           `json:"limits"`
	DNS           DNS              `json:"dns"`
	Retry         Retry            `json:"retry"`
	CIDRWhitelist []CIDREntry      `json:"cidr_whitelist"`
	Providers     []Provider       `json:"providers"`
	Whitelist     []WhitelistEntry `json:"whitelist"`
	Routes        []Route          `json:"routes"`

	// Derived lookup tables, computed at load time.
	routeExact  map[string]*Route
	routeDomain map[string]*Route
}

// Load reads the configuration file, decodes it, rejects unknown fields,
// validates it, and precomputes the derived values.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()

	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := c.Prepare(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Prepare normalises the case of every comparison key, parses the CIDR
// entries, and builds the route lookup tables. Load calls it. A caller that
// constructs a Config in code must call it before use. Parse failures are
// reported by Validate rather than here, so that one run reports every
// fault in the file.
func (c *Config) Prepare() error {
	c.Hostname = strings.ToLower(strings.TrimSpace(c.Hostname))

	for i := range c.CIDRWhitelist {
		e := &c.CIDRWhitelist[i]
		if _, n, err := net.ParseCIDR(e.CIDR); err == nil {
			e.Net = n
		}
		for j := range e.Domains {
			e.Domains[j] = strings.ToLower(e.Domains[j])
		}
	}

	for i := range c.Providers {
		p := &c.Providers[i]
		for j := range p.PTRSuffixes {
			p.PTRSuffixes[j] = strings.ToLower(strings.TrimPrefix(p.PTRSuffixes[j], "."))
		}
		for j := range p.Domains {
			p.Domains[j] = strings.ToLower(p.Domains[j])
		}
	}

	for i := range c.Whitelist {
		c.Whitelist[i].Value = strings.ToLower(c.Whitelist[i].Value)
	}

	c.routeExact = make(map[string]*Route)
	c.routeDomain = make(map[string]*Route)
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Match == "" {
			r.Match = MatchAddress
		}
		r.Recipient = strings.ToLower(r.Recipient)
		switch r.Match {
		case MatchAddress:
			c.routeExact[r.Recipient] = r
		case MatchDomain:
			c.routeDomain[r.Recipient] = r
		}
	}
	return nil
}

// Validate applies every rule in section 13.2 of the specification. Faults
// accumulate so that one run reports all of them.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf(format, a...))
	}

	if c.Hostname == "" {
		add("hostname is empty")
	}
	if len(c.Listeners) == 0 {
		add("no listeners configured")
	}

	needTLS := false
	for i, l := range c.Listeners {
		if _, _, err := net.SplitHostPort(l.Addr); err != nil {
			add("listener %d: address %q does not parse: %v", i, l.Addr, err)
		}
		if l.TLS == "" {
			add("listener %d: tls mode is empty", i)
		}
		if l.TLS == TLSStartTLS || l.TLS == TLSImplicit {
			needTLS = true
		}
	}
	if needTLS {
		if c.TLS.Cert == "" || c.TLS.Key == "" {
			add("a listener requires tls but the certificate or key path is empty")
		}
	}

	if c.Limits.MaxMessageBytes <= 0 {
		add("limits.max_message_bytes must be greater than zero")
	}
	if c.Limits.MaxConnections <= 0 {
		add("limits.max_connections must be greater than zero")
	}
	if c.Limits.CommandTimeoutSec <= 0 {
		add("limits.command_timeout_sec must be greater than zero")
	}
	if c.Limits.SessionTimeoutSec <= 0 {
		add("limits.session_timeout_sec must be greater than zero")
	}
	if c.DNS.TimeoutSec <= 0 {
		add("dns.timeout_sec must be greater than zero")
	}

	if c.Retry.Enabled {
		if c.Retry.IntervalSec <= 0 {
			add("retry.interval_sec must be greater than zero")
		}
		if c.Retry.ExpireSec <= c.Retry.IntervalSec {
			add("retry.expire_sec must be greater than retry.interval_sec")
		}
		if c.Retry.MaxEntries <= 0 {
			add("retry.max_entries must be greater than zero")
		}
		if c.Retry.MaxBytes <= 0 {
			add("retry.max_bytes must be greater than zero")
		}
	}

	for i, e := range c.CIDRWhitelist {
		if e.Net == nil {
			add("cidr_whitelist %d: %q does not parse as a network", i, e.CIDR)
		}
	}

	for i, p := range c.Providers {
		if p.Name == "" {
			add("providers %d: name is empty", i)
		}
		if len(p.PTRSuffixes) == 0 {
			add("providers %d: ptr_suffixes is empty", i)
		}
		if len(p.Domains) == 0 {
			add("providers %d: domains is empty", i)
		}
	}

	for i, w := range c.Whitelist {
		if w.Match == "" {
			add("whitelist %d: match is empty", i)
		}
		if w.Value == "" {
			add("whitelist %d: value is empty", i)
		}
		if w.Match == MatchAddress && !strings.Contains(w.Value, "@") {
			add("whitelist %d: address match %q contains no at sign", i, w.Value)
		}
	}

	if len(c.Routes) == 0 {
		add("no routes configured")
	}
	seen := make(map[string]bool)
	for i := range c.Routes {
		r := &c.Routes[i]
		key := string(r.Match) + ":" + r.Recipient
		if seen[key] {
			add("routes %d: duplicate recipient %q for match type %q",
				i, r.Recipient, r.Match)
		}
		seen[key] = true

		if r.Recipient == "" {
			add("routes %d: recipient is empty", i)
		}
		if r.TimeoutSec <= 0 {
			add("routes %d: timeout_sec must be greater than zero", i)
		}
		switch r.Type {
		case RouteCommand:
			if r.Path == "" {
				add("routes %d: command route has no path", i)
			}
		case RouteWebhook:
			if !strings.HasPrefix(r.URL, "https://") &&
				!strings.HasPrefix(r.URL, "http://") {
				add("routes %d: webhook url %q is not an http url", i, r.URL)
			}
			if r.Secret == "" {
				add("routes %d: webhook route has no secret", i)
			}
		case RouteForward:
			if r.Host == "" {
				add("routes %d: forward route has no host", i)
			}
			if r.Port <= 0 || r.Port > 65535 {
				add("routes %d: forward port %d is out of range", i, r.Port)
			}
		default:
			add("routes %d: type is empty", i)
		}
	}

	return errors.Join(errs...)
}

// LookupRoute returns the route for a forward-path, or nil when the address
// has no route. An exact address entry is selected before a domain entry.
func (c *Config) LookupRoute(addr string) *Route {
	addr = strings.ToLower(addr)
	if r, ok := c.routeExact[addr]; ok {
		return r
	}
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return nil
	}
	if r, ok := c.routeDomain[addr[at+1:]]; ok {
		return r
	}
	return nil
}
