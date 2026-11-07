package widgets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// The guarded client (SPEC-status-widgets AC-017, AC-018, §8.2) is the only
// HTTP client a provider gets, and the favicon fetcher uses it too (#90).
//
// The server fetches admin-supplied addresses, and homelab apps live on
// private addresses, so reaching private hosts is the feature. The controls
// bound what comes back and where a request may go:
//
//   - http and https only;
//   - no redirects followed (a 3xx is the `redirect` error). Go forwards custom
//     headers such as X-Api-Key across redirects, so following one could hand a
//     key to another host;
//   - link-local (169.254.0.0/16, fe80::/10, which covers the cloud metadata
//     endpoints), fd00:ec2::254, unspecified and multicast addresses are
//     refused. The check runs on the RESOLVED address: the dialer resolves the
//     host itself, refuses the whole answer if any address is blocked, and
//     dials only the checked IP, so a rebinding hostname can't get past it. A
//     Control hook re-checks the address the socket actually connects to;
//   - proxies from the environment are ignored, so HTTP(S)_PROXY can't route a
//     request around the guard;
//   - 5s per request, a 1 MiB body cap, and errors that carry only a code.

// RequestTimeout bounds every request (AC-017).
const RequestTimeout = 5 * time.Second

// MaxBodyBytes caps every response body (AC-017).
const MaxBodyBytes = 1 << 20

// ErrTooLarge marks a body over its cap. It is also a bad_response.
var ErrTooLarge = &tooLargeError{}

type tooLargeError struct{}

func (*tooLargeError) Error() string { return "widget: bad_response" }

// Is makes a too-large error match both ErrTooLarge and ErrBadResponse.
func (*tooLargeError) Is(target error) bool {
	return target == ErrTooLarge || errors.Is(ErrBadResponse, target)
}

// errBlockedDial is what the dial path returns for a refused address; Do maps
// it to ErrBlockedAddress.
var errBlockedDial = errors.New("widget: blocked address")

// Resolver resolves a host name. *net.Resolver satisfies it; tests inject a
// fake to simulate DNS rebinding without any DNS.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DialFunc dials an already-checked IP:port.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Option configures a Client.
type Option func(*Client)

// WithResolver replaces the system resolver.
func WithResolver(r Resolver) Option { return func(c *Client) { c.resolver = r } }

// WithDialer replaces the final dial of a checked address (tests count dials
// with it). The address guard runs before it either way.
func WithDialer(d DialFunc) Option { return func(c *Client) { c.dial = d } }

// WithTimeout replaces the per-request timeout (tests).
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// Client is the guarded outbound HTTP client. It is safe for concurrent use.
type Client struct {
	resolver Resolver
	dial     DialFunc
	timeout  time.Duration
	verified *http.Client
	insecure *http.Client
}

// NewClient builds a guarded client.
func NewClient(opts ...Option) *Client {
	c := &Client{resolver: net.DefaultResolver, timeout: RequestTimeout}
	for _, o := range opts {
		o(c)
	}
	if c.dial == nil {
		d := &net.Dialer{Timeout: c.timeout, Control: controlCheck}
		c.dial = d.DialContext
	}
	c.verified = c.httpClient(false)
	c.insecure = c.httpClient(true)
	return c
}

func (c *Client) httpClient(insecure bool) *http.Client {
	tr := &http.Transport{
		Proxy:                  nil, // never from the environment
		DialContext:            c.guardedDial,
		TLSHandshakeTimeout:    c.timeout,
		ResponseHeaderTimeout:  c.timeout,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConns:           16,
		IdleConnTimeout:        90 * time.Second,
		ForceAttemptHTTP2:      true,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if insecure {
		// Only when the admin ticked "Allow self-signed certificate" on this
		// widget (AC-017).
		tr.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // admin opt-in per widget
	}
	return &http.Client{
		Transport: tr,
		Timeout:   c.timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // a 3xx comes back as-is and maps to `redirect`
		},
	}
}

// guardedDial resolves addr's host, refuses the answer if any address is
// blocked, and dials the first checked IP that answers.
func (c *Client) guardedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		answers, err := c.resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, a := range answers {
			ips = append(ips, a.IP)
		}
	}
	if len(ips) == 0 {
		return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
	}
	for _, ip := range ips {
		if IsBlockedIP(ip) {
			return nil, errBlockedDial
		}
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := c.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// controlCheck is the dialer's Control hook: the last check, on the address
// the socket is about to connect to.
func controlCheck(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errBlockedDial
	}
	ip := net.ParseIP(host)
	if ip == nil || IsBlockedIP(ip) {
		return errBlockedDial
	}
	return nil
}

var (
	metadataV6 = net.ParseIP("fd00:ec2::254")
	nat64      = mustCIDR("64:ff9b::/96")
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// IsBlockedIP reports whether the guard refuses ip (AC-018): link-local
// (169.254.0.0/16, fe80::/10) including through IPv4-mapped and NAT64 forms,
// the AWS IPv6 metadata address fd00:ec2::254, and the unspecified and
// multicast addresses, which name no app. Private and loopback addresses are
// allowed: they are where homelab apps live (Q5 a).
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if nat64.Contains(ip) {
		ip = net.IP(ip.To16()[12:16])
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return true
	case ip.IsUnspecified(), ip.IsMulticast():
		return true
	case ip.Equal(metadataV6):
		return true
	}
	return false
}

// Request is one guarded request.
type Request struct {
	Method      string // "" = GET
	URL         *url.URL
	Header      http.Header
	Body        []byte
	InsecureTLS bool
	// MaxBody caps the body read; 0 = MaxBodyBytes. A larger cap than
	// MaxBodyBytes is honoured (Phase 2 streams one exception).
	MaxBody int64
	// AllowTruncate returns the first MaxBody bytes instead of failing, for a
	// caller that only scans a prefix (the favicon page).
	AllowTruncate bool
}

// Response is a guarded response. Status is always 1xx/2xx/4xx/5xx: a 3xx is
// the redirect error.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Do performs one guarded request. Every error it returns is a typed *Error
// (or ErrTooLarge): it never carries the URL, a header or upstream text.
func (c *Client) Do(ctx context.Context, req Request) (Response, error) {
	if req.URL == nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Host == "" {
		return Response{}, ErrBlockedAddress
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, method, req.URL.String(), body)
	if err != nil {
		return Response{}, ErrBadResponse
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	if hr.Header.Get("User-Agent") == "" {
		hr.Header.Set("User-Agent", "Homepad")
	}
	hc := c.verified
	if req.InsecureTLS {
		hc = c.insecure
	}
	resp, err := hc.Do(hr)
	if err != nil {
		return Response{}, mapTransportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return Response{}, ErrRedirect
	}
	limit := req.MaxBody
	if limit <= 0 {
		limit = MaxBodyBytes
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Response{}, mapTransportError(ctx, err)
	}
	if int64(len(b)) > limit {
		if !req.AllowTruncate {
			return Response{}, ErrTooLarge
		}
		b = b[:limit]
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: b}, nil
}

// GetJSON GETs u and decodes a 2xx JSON body into dst. 401/403 → auth; any
// other non-2xx, or a body that isn't the JSON dst expects → bad_response.
func (c *Client) GetJSON(ctx context.Context, u *url.URL, h http.Header, insecureTLS bool, dst any) error {
	resp, err := c.Do(ctx, Request{URL: u, Header: h, InsecureTLS: insecureTLS})
	if err != nil {
		return err
	}
	return DecodeJSON(resp, dst)
}

// DecodeJSON maps a response's status and decodes its JSON body into dst.
func DecodeJSON(resp Response, dst any) error {
	switch {
	case resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden:
		return ErrAuth
	case resp.Status < 200 || resp.Status > 299:
		return ErrBadResponse
	}
	if err := json.Unmarshal(resp.Body, dst); err != nil {
		return ErrBadResponse
	}
	return nil
}

// mapTransportError turns a transport failure into a code. It inspects the
// error's type only; the error itself (which holds the URL) is dropped.
func mapTransportError(ctx context.Context, err error) error {
	if errors.Is(err, errBlockedDial) {
		return ErrBlockedAddress
	}
	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		invalid          x509.CertificateInvalidError
		verify           *tls.CertificateVerificationError
		record           tls.RecordHeaderError
	)
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) ||
		errors.As(err, &verify) || errors.As(err, &record) {
		return ErrTLS
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return ErrTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrTimeout
	}
	if strings.Contains(err.Error(), "tls:") {
		return ErrTLS
	}
	return ErrUnreachable
}
