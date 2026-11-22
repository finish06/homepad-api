package api

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// SPEC-signin-hardening §2.2 — which address is "the client" for the sign-in
// limits. By default it is the TCP peer, and X-Forwarded-For and X-Real-IP
// are ignored: anyone can send them. HOMEPAD_TRUSTED_PROXIES names the
// proxies whose X-Forwarded-For is believed.

// trustedRefreshEvery is how often hostnames in HOMEPAD_TRUSTED_PROXIES are
// resolved again, so a proxy container that moves stays trusted.
const trustedRefreshEvery = 5 * time.Minute

// TrustedRefreshEvery is trustedRefreshEvery, for main.
const TrustedRefreshEvery = trustedRefreshEvery

// trustedRetryEvery is how often a hostname that has never resolved is tried
// again. At startup the proxy container may not be up yet; until its name
// resolves, every client behind it shares one limit.
const trustedRetryEvery = 10 * time.Second

// TrustedRetryEvery is trustedRetryEvery, for main.
const TrustedRetryEvery = trustedRetryEvery

// trustedLookupTimeout bounds one hostname lookup.
const trustedLookupTimeout = 5 * time.Second

// TrustedProxies is HOMEPAD_TRUSTED_PROXIES, parsed: addresses and CIDR
// ranges, plus hostnames resolved at startup and every trustedRefreshEvery.
// The zero value, and nil, trust nothing.
type TrustedProxies struct {
	prefixes []netip.Prefix
	hosts    []string
	lookup   func(ctx context.Context, host string) ([]netip.Addr, error)

	mu       sync.RWMutex
	resolved map[string][]netip.Addr
	// failLogged is when each hostname's failed lookup was last logged, so
	// the short retry logs at most once per trustedRefreshEvery.
	failLogged map[string]time.Time
}

// ParseTrustedProxies parses a comma-separated list of addresses, CIDR
// ranges and hostnames. Blank entries are skipped; an empty list trusts no
// one. Anything else is an error that names the variable: main stops the API
// on it (AC-008). Hostnames are not resolved here; call Refresh.
func ParseTrustedProxies(raw string) (*TrustedProxies, error) {
	return parseTrustedProxies(raw, func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	})
}

func parseTrustedProxies(raw string, lookup func(context.Context, string) ([]netip.Addr, error)) (*TrustedProxies, error) {
	tp := &TrustedProxies{lookup: lookup, resolved: map[string][]netip.Addr{}, failLogged: map[string]time.Time{}}
	for _, entry := range strings.Split(raw, ",") {
		e := strings.TrimSpace(entry)
		if e == "" {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			tp.prefixes = append(tp.prefixes, unmapPrefix(p).Masked())
			continue
		}
		if a, err := netip.ParseAddr(e); err == nil {
			a = a.Unmap()
			tp.prefixes = append(tp.prefixes, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		if validHostname(e) {
			tp.hosts = append(tp.hosts, strings.ToLower(e))
			continue
		}
		return nil, fmt.Errorf("HOMEPAD_TRUSTED_PROXIES: %q is not an address, a CIDR range or a hostname", e)
	}
	return tp, nil
}

func unmapPrefix(p netip.Prefix) netip.Prefix {
	if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
		return netip.PrefixFrom(a.Unmap(), p.Bits()-96)
	}
	return p
}

// validHostname accepts DNS names as Docker and Kubernetes use them: labels
// of letters, digits, hyphens and underscores, 1 to 63 characters, not
// starting or ending with a hyphen. A name whose last label is all digits
// is a mistyped address, not a hostname.
func validHostname(h string) bool {
	if len(h) > 253 {
		return false
	}
	labels := strings.Split(strings.TrimSuffix(h, "."), ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			ok := c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
			if !ok {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	return strings.Trim(last, "0123456789") != ""
}

// HasHostnames reports whether any entry needs resolving.
func (tp *TrustedProxies) HasHostnames() bool { return tp != nil && len(tp.hosts) > 0 }

// Size is the number of entries, for the startup log line.
func (tp *TrustedProxies) Size() int {
	if tp == nil {
		return 0
	}
	return len(tp.prefixes) + len(tp.hosts)
}

// Refresh resolves every hostname. A failed lookup keeps that name's last
// good addresses; until a name has resolved once, it trusts nothing.
func (tp *TrustedProxies) Refresh(ctx context.Context) {
	if !tp.HasHostnames() {
		return
	}
	for _, h := range tp.hosts {
		lctx, cancel := context.WithTimeout(ctx, trustedLookupTimeout)
		addrs, err := tp.lookup(lctx, h)
		cancel()
		if err != nil || len(addrs) == 0 {
			if ctx.Err() == nil && tp.logFailure(h) {
				log.Printf("trusted proxies: can't resolve %q (HOMEPAD_TRUSTED_PROXIES): %v", h, err)
			}
			continue
		}
		out := make([]netip.Addr, len(addrs))
		for i, a := range addrs {
			out[i] = a.Unmap()
		}
		tp.mu.Lock()
		tp.resolved[h] = out
		tp.mu.Unlock()
	}
}

// logFailure reports whether a failed lookup of h should be logged: the
// first time, then at most once per trustedRefreshEvery.
func (tp *TrustedProxies) logFailure(h string) bool {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	now := time.Now()
	if last, ok := tp.failLogged[h]; ok && now.Sub(last) < trustedRefreshEvery {
		return false
	}
	tp.failLogged[h] = now
	return true
}

// allResolved reports whether every hostname has resolved at least once.
func (tp *TrustedProxies) allResolved() bool {
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	for _, h := range tp.hosts {
		if _, ok := tp.resolved[h]; !ok {
			return false
		}
	}
	return true
}

// Run refreshes the hostnames until ctx ends: every retry while some
// hostname has never resolved, then every interval.
func (tp *TrustedProxies) Run(ctx context.Context, every, retry time.Duration) {
	if !tp.HasHostnames() {
		return
	}
	for {
		wait := every
		if !tp.allResolved() {
			wait = retry
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			tp.Refresh(ctx)
		}
	}
}

func (tp *TrustedProxies) contains(a netip.Addr) bool {
	if tp == nil {
		return false
	}
	for _, p := range tp.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	if len(tp.hosts) == 0 {
		return false
	}
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	for _, addrs := range tp.resolved {
		for _, r := range addrs {
			if r == a {
				return true
			}
		}
	}
	return false
}

// clientAddr is the key the per-client limits use (§2.2). The client is the
// TCP peer, unless the peer is a trusted proxy: then it is the right-most
// X-Forwarded-For entry that isn't a trusted proxy. An entry that doesn't
// parse stops the walk at the last trusted hop, so junk can't pick a key.
// X-Real-IP is never read. The key is clientKey of that address.
func clientAddr(r *http.Request, tp *TrustedProxies) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap().WithZone("")
	if !tp.contains(peer) {
		return clientKey(peer)
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	last := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseHop(hops[i])
		if !ok {
			break
		}
		if !tp.contains(a) {
			return clientKey(a)
		}
		last = a
	}
	return clientKey(last)
}

// clientKey is the limit key for a client address. An IPv4 address is its
// own key. An IPv6 address is keyed on its /64: one home or cloud host
// usually holds a whole /64, and could otherwise get a fresh bucket for every
// address in it. a is already unmapped, so IPv4-mapped IPv6 is IPv4 here.
func clientKey(a netip.Addr) string {
	if a.Is4() {
		return a.String()
	}
	return netip.PrefixFrom(a, 64).Masked().String()
}

// parseHop reads one X-Forwarded-For entry: an address, optionally with a
// port, IPv6 optionally in brackets.
func parseHop(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap().WithZone(""), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap().WithZone(""), true
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		if a, err := netip.ParseAddr(s[1 : len(s)-1]); err == nil {
			return a.Unmap().WithZone(""), true
		}
	}
	return netip.Addr{}, false
}
