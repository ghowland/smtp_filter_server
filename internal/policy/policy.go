// Package policy implements the sender admission decision. It performs the
// forward-confirmed reverse DNS procedure and the SPF evaluation through a
// resolver interface that it declares, so that the rules can be tested
// without network access.
package policy

import (
	"context"
	"net"
	"strings"

	"smtpfilter/internal/config"
)

// SPFResult is the outcome of an SPF evaluation, as defined by RFC 7208.
type SPFResult string

const (
	SPFPass      SPFResult = "PASS"
	SPFFail      SPFResult = "FAIL"
	SPFSoftFail  SPFResult = "SOFTFAIL"
	SPFNeutral   SPFResult = "NEUTRAL"
	SPFNone      SPFResult = "NONE"
	SPFTempError SPFResult = "TEMPERROR"
	SPFPermError SPFResult = "PERMERROR"
	// SPFSkipped means the matched entry set require_spf to false.
	SPFSkipped SPFResult = "SKIPPED"
)

// Resolver supplies the two DNS operations and the SPF evaluation that the
// admission rules require.
type Resolver interface {
	// LookupAddr performs a PTR query on an address.
	LookupAddr(ctx context.Context, addr string) ([]string, error)
	// LookupIPAddr performs an A and AAAA query on a hostname.
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	// CheckSPF evaluates the SPF record of domain against ip.
	CheckSPF(ctx context.Context, ip net.IP, domain, sender string) SPFResult
}

// Outcome is the disposition of one admission decision.
type Outcome int

const (
	// Admit permits the transaction to continue.
	Admit Outcome = iota
	// RejectPermanent produces a 550 reply and closes the connection.
	RejectPermanent
	// RejectTemporary produces a 451 reply and closes the connection.
	RejectTemporary
)

// Decision records the outcome and the evidence that produced it, so that
// the log carries the reason a sender was refused.
type Decision struct {
	Outcome Outcome
	Rule    string
	SPF     SPFResult
	PTRName string
	Reason  string
}

// DomainOf returns the part of an address after the final at sign, in lower
// case. It returns an empty string when the address contains no at sign.
func DomainOf(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return ""
	}
	return strings.ToLower(addr[at+1:])
}

// Decide evaluates rules A through D in fixed order and stops at the first
// rule that matches. The order does not depend on configuration.
func Decide(ctx context.Context, r Resolver, cfg *config.Config,
	ip net.IP, reversePath string) Decision {

	sender := strings.ToLower(strings.TrimSpace(reversePath))
	domain := DomainOf(sender)

	// Rule A. Containment in a configured network. No DNS query.
	for _, e := range cfg.CIDRWhitelist {
		if e.Net == nil || !e.Net.Contains(ip) {
			continue
		}
		if len(e.Domains) > 0 && !contains(e.Domains, domain) {
			continue
		}
		return Decision{Outcome: Admit, Rule: "cidr", SPF: SPFSkipped}
	}

	// Rule B. Provider identified by forward-confirmed reverse DNS.
	if len(cfg.Providers) > 0 {
		name, ok := FCrDNS(ctx, r, ip)
		if ok {
			for _, p := range cfg.Providers {
				if !suffixMatch(name, p.PTRSuffixes) {
					continue
				}
				if !contains(p.Domains, domain) {
					continue
				}
				if !p.SPFRequired() {
					return Decision{Outcome: Admit, Rule: "provider:" + p.Name,
						SPF: SPFSkipped, PTRName: name}
				}
				res := r.CheckSPF(ctx, ip, domain, sender)
				d := mapSPF(res)
				d.Rule = "provider:" + p.Name
				d.PTRName = name
				return d
			}
		}
	}

	// Rule C. Reverse-path present in the whitelist.
	for _, w := range cfg.Whitelist {
		if !whitelistMatch(w, sender, domain) {
			continue
		}
		if !w.SPFRequired() {
			return Decision{Outcome: Admit, Rule: "whitelist:" + w.Value,
				SPF: SPFSkipped}
		}
		res := r.CheckSPF(ctx, ip, domain, sender)
		d := mapSPF(res)
		d.Rule = "whitelist:" + w.Value
		return d
	}

	// Rule D. No rule matched.
	return Decision{
		Outcome: RejectPermanent,
		Rule:    "default",
		Reason:  "sender not whitelisted",
	}
}

// FCrDNS performs the forward-confirmed reverse DNS procedure. It returns
// the first hostname whose forward lookup contains the original address.
// It makes no statement about the reverse-path.
func FCrDNS(ctx context.Context, r Resolver, ip net.IP) (string, bool) {
	names, err := r.LookupAddr(ctx, ip.String())
	if err != nil || len(names) == 0 {
		return "", false
	}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSuffix(n, "."))
		addrs, err := r.LookupIPAddr(ctx, n)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if a.IP.Equal(ip) {
				return n, true
			}
		}
	}
	return "", false
}

// mapSPF applies the result table of section 6.5 of the specification.
// TempError is a temporary rejection because a DNS failure is not evidence
// of forgery, and a permanent rejection would discard valid mail during a
// transient network fault.
func mapSPF(res SPFResult) Decision {
	switch res {
	case SPFPass:
		return Decision{Outcome: Admit, SPF: res}
	case SPFTempError:
		return Decision{Outcome: RejectTemporary, SPF: res,
			Reason: "spf temporary error"}
	default:
		return Decision{Outcome: RejectPermanent, SPF: res,
			Reason: "spf result " + string(res)}
	}
}

// whitelistMatch compares one whitelist entry to a reverse-path. A domain
// entry matches the domain itself and any subdomain of it.
func whitelistMatch(w config.WhitelistEntry, sender, domain string) bool {
	switch w.Match {
	case config.MatchAddress:
		return sender == w.Value
	case config.MatchDomain:
		return domain == w.Value || strings.HasSuffix(domain, "."+w.Value)
	}
	return false
}

// suffixMatch reports whether a hostname equals one of the suffixes or is a
// subdomain of one of them. A bare suffix comparison is not used, because
// it would match a name such as "notgoogle.com" against "google.com".
func suffixMatch(name string, suffixes []string) bool {
	for _, s := range suffixes {
		if name == s || strings.HasSuffix(name, "."+s) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

