package policy

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/mileusna/spf"
)

// NetResolver is the production Resolver. It wraps one net.Resolver, which
// is safe for concurrent use, and the SPF library. No result is cached.
type NetResolver struct {
	r       *net.Resolver
	timeout time.Duration
}

// NewNetResolver returns a resolver that applies the given deadline to
// every query. The server address, when not empty, is written to the
// package-level variable that the SPF library uses to select its resolver.
func NewNetResolver(timeout time.Duration, server string) *NetResolver {
	if server != "" {
		spf.DNSServer = server
	}
	return &NetResolver{r: net.DefaultResolver, timeout: timeout}
}

// LookupAddr performs a PTR query under the configured deadline.
func (n *NetResolver) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	return n.r.LookupAddr(ctx, addr)
}

// LookupIPAddr performs an A and AAAA query under the configured deadline.
func (n *NetResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	return n.r.LookupIPAddr(ctx, host)
}

// CheckSPF evaluates the SPF record of domain against ip.
//
// The library accepts no context and exposes no resolver interface, so the
// deadline is applied around the call rather than inside it. The channel
// buffer of one is mandatory: without it the inner goroutine would block
// permanently on the send after a timeout and would leak. With the buffer
// the goroutine terminates when the underlying query completes and is then
// collected, while the session is released at the deadline either way.
func (n *NetResolver) CheckSPF(ctx context.Context, ip net.IP, domain, sender string) SPFResult {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()

	ch := make(chan SPFResult, 1)
	go func() {
		ch <- convertSPF(spf.CheckHost(ip, domain, sender, ""))
	}()

	select {
	case r := <-ch:
		return r
	case <-ctx.Done():
		return SPFTempError
	}
}

// convertSPF translates the library result into the type declared by this
// package. The comparison is made on the string value, which the library
// documents, rather than on its exported identifiers. This confines the
// library to one function and permits a substitution without further
// change elsewhere.
func convertSPF(r spf.Result) SPFResult {
	switch strings.ToUpper(strings.TrimSpace(string(r))) {
	case "PASS":
		return SPFPass
	case "FAIL":
		return SPFFail
	case "SOFTFAIL":
		return SPFSoftFail
	case "NEUTRAL":
		return SPFNeutral
	case "NONE":
		return SPFNone
	case "TEMPERROR":
		return SPFTempError
	case "PERMERROR":
		return SPFPermError
	}
	return SPFPermError
}

