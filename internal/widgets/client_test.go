package widgets

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-status-widgets AC-017, AC-018 — every widget request goes through one
// guarded client. These tests use httptest fakes only: no live calls.

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// fakeResolver answers fixed IPs per host, so a test can make a hostname
// "rebind" to a metadata address without any DNS.
type fakeResolver map[string][]string

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := f[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := make([]net.IPAddr, 0, len(ips))
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

// recordingDialer counts every TCP dial the client attempts, so a test can
// prove a refused address never got a connection.
type recordingDialer struct {
	n     atomic.Int32
	addrs []string
}

func (d *recordingDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.n.Add(1)
	d.addrs = append(d.addrs, addr)
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

func TestClient_GetJSON_DecodesAndSendsHeaders(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"totalCount": 4}`))
	}))
	defer srv.Close()

	var out struct {
		TotalCount *float64 `json:"totalCount"`
	}
	err := NewClient().GetJSON(context.Background(), mustURL(t, srv.URL+"/api/v3/queue/status"),
		http.Header{"X-Api-Key": {"k-123"}}, false, &out)
	require.NoError(t, err)
	require.NotNil(t, out.TotalCount)
	assert.Equal(t, 4.0, *out.TotalCount)
	assert.Equal(t, "k-123", gotKey)
}

func TestClient_NoRedirectsFollowed(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer target.Close()
	for _, code := range []int{301, 302, 303, 307, 308} {
		src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/login", code)
		}))
		var out map[string]any
		err := NewClient().GetJSON(context.Background(), mustURL(t, src.URL), http.Header{"X-Api-Key": {"k"}}, false, &out)
		src.Close()
		assert.ErrorIs(t, err, ErrRedirect, "status %d", code)
	}
	assert.Zero(t, targetHits.Load(), "the redirect target must never be requested")
}

func TestClient_StatusMapping(t *testing.T) {
	cases := map[int]error{
		401: ErrAuth,
		403: ErrAuth,
		404: ErrBadResponse,
		500: ErrBadResponse,
		503: ErrBadResponse,
	}
	for status, want := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"upstream text that must not leak"}`))
		}))
		var out map[string]any
		err := NewClient().GetJSON(context.Background(), mustURL(t, srv.URL), nil, false, &out)
		srv.Close()
		assert.ErrorIs(t, err, want, "status %d", status)
		if err != nil {
			assert.NotContains(t, err.Error(), "upstream text")
		}
	}
}

func TestClient_BadJSONIsBadResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>login</html>`))
	}))
	defer srv.Close()
	var out map[string]any
	err := NewClient().GetJSON(context.Background(), mustURL(t, srv.URL), nil, false, &out)
	assert.ErrorIs(t, err, ErrBadResponse)
}

func TestClient_BodyCappedAt1MiB(t *testing.T) {
	assert.Equal(t, int64(1<<20), int64(MaxBodyBytes))
	big := strings.Repeat("x", MaxBodyBytes+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"a":"` + big + `"}`))
	}))
	defer srv.Close()

	var out map[string]any
	err := NewClient().GetJSON(context.Background(), mustURL(t, srv.URL), nil, false, &out)
	assert.ErrorIs(t, err, ErrBadResponse)
	assert.ErrorIs(t, err, ErrTooLarge)

	// A caller that only scans a prefix (the favicon page) may opt in to a
	// truncated read instead.
	resp, err := NewClient().Do(context.Background(), Request{URL: mustURL(t, srv.URL), MaxBody: 1000, AllowTruncate: true})
	require.NoError(t, err)
	assert.Len(t, resp.Body, 1000)
}

func TestClient_Timeout(t *testing.T) {
	assert.Equal(t, 5*time.Second, RequestTimeout)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	var out map[string]any
	err := NewClient(WithTimeout(100*time.Millisecond)).GetJSON(context.Background(), mustURL(t, srv.URL), nil, false, &out)
	assert.ErrorIs(t, err, ErrTimeout)
}

func TestClient_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close() // nothing listens there now

	var out map[string]any
	err := NewClient().GetJSON(context.Background(), mustURL(t, addr), nil, false, &out)
	assert.ErrorIs(t, err, ErrUnreachable)

	err = NewClient(WithResolver(fakeResolver{})).GetJSON(context.Background(), mustURL(t, "http://nowhere.home.arpa:8096/"), nil, false, &out)
	assert.ErrorIs(t, err, ErrUnreachable, "an unknown host is unreachable")
}

func TestClient_TLSVerifiedUnlessInsecure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var out map[string]any
	err := NewClient().GetJSON(context.Background(), mustURL(t, srv.URL), nil, false, &out)
	assert.ErrorIs(t, err, ErrTLS, "a self-signed certificate is refused by default")

	err = NewClient().GetJSON(context.Background(), mustURL(t, srv.URL), nil, true, &out)
	assert.NoError(t, err, "Allow self-signed certificate accepts it")
}

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"169.254.169.254", "169.254.0.1", "169.254.255.255",
		"fe80::1", "fe80::a9fe:a9fe", "febf::1",
		"fd00:ec2::254",
		"::ffff:169.254.169.254", // IPv4-mapped
		"64:ff9b::a9fe:a9fe",     // NAT64 of 169.254.169.254
		"0.0.0.0", "::",          // unspecified
		"224.0.0.1", "ff02::1", // multicast
	}
	allowed := []string{
		"127.0.0.1", "::1", "10.0.0.5", "172.16.0.1", "192.168.1.20",
		"100.64.0.1", "fd12:3456::1", "fd00:ec2::253", "93.184.216.34", "2001:db8::1",
	}
	for _, s := range blocked {
		assert.True(t, IsBlockedIP(net.ParseIP(s)), "%s must be blocked", s)
	}
	for _, s := range allowed {
		assert.False(t, IsBlockedIP(net.ParseIP(s)), "%s must be allowed (private and loopback are the feature)", s)
	}
}

func TestClient_MetadataAddressRefusedBeforeAnyConnection(t *testing.T) {
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]:8080/",
		"http://[fd00:ec2::254]/",
		"http://[::ffff:169.254.169.254]/",
	} {
		d := &recordingDialer{}
		var out map[string]any
		err := NewClient(WithDialer(d.dial)).GetJSON(context.Background(), mustURL(t, raw), nil, false, &out)
		assert.ErrorIs(t, err, ErrBlockedAddress, raw)
		assert.Zero(t, d.n.Load(), "%s: no connection may be attempted", raw)
	}
}

// AC-018 — the check is on the RESOLVED address, so a hostname that resolves
// (or rebinds) to a metadata address is refused too.
func TestClient_RebindingHostnameRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	res := fakeResolver{
		"rebind.example.com": {"169.254.169.254"},
		"mixed.example.com":  {"127.0.0.1", "169.254.169.254"},
		"app.home.arpa":      {"127.0.0.1"},
	}
	for _, host := range []string{"rebind.example.com", "mixed.example.com"} {
		d := &recordingDialer{}
		var out map[string]any
		err := NewClient(WithResolver(res), WithDialer(d.dial)).GetJSON(context.Background(),
			mustURL(t, "http://"+host+":"+port+"/"), nil, false, &out)
		assert.ErrorIs(t, err, ErrBlockedAddress, host)
		assert.Zero(t, d.n.Load(), "%s: no connection may be attempted", host)
	}

	// A private/loopback answer is allowed, and the dial goes to the checked IP.
	d := &recordingDialer{}
	var out map[string]any
	err := NewClient(WithResolver(res), WithDialer(d.dial)).GetJSON(context.Background(),
		mustURL(t, "http://app.home.arpa:"+port+"/"), nil, false, &out)
	require.NoError(t, err)
	assert.Equal(t, []string{"127.0.0.1:" + port}, d.addrs)
}

// The dialer's Control hook re-checks the address the socket connects to,
// as a second line behind the resolver check.
func TestControlHookRefusesBlockedAddress(t *testing.T) {
	assert.ErrorIs(t, controlCheck("tcp4", "169.254.169.254:80", nil), errBlockedDial)
	assert.ErrorIs(t, controlCheck("tcp6", "[fe80::1]:80", nil), errBlockedDial)
	assert.NoError(t, controlCheck("tcp4", "127.0.0.1:80", nil))
	assert.NoError(t, controlCheck("tcp4", "10.1.2.3:8096", nil))
}

func TestClient_OnlyHTTPAndHTTPS(t *testing.T) {
	for _, raw := range []string{"ftp://example.com/x", "file:///etc/passwd", "gopher://example.com/"} {
		d := &recordingDialer{}
		var out map[string]any
		err := NewClient(WithDialer(d.dial)).GetJSON(context.Background(), mustURL(t, raw), nil, false, &out)
		assert.ErrorIs(t, err, ErrBlockedAddress, raw)
		assert.Zero(t, d.n.Load())
	}
}

// An HTTP(S)_PROXY in the environment must not route widget requests around
// the address guard.
func TestClient_IgnoresProxyEnvironment(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"direct":true}`))
	}))
	defer srv.Close()
	// A non-loopback name, so Go's own "never proxy localhost" rule isn't
	// what makes the test pass.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	var out map[string]any
	err := NewClient(WithResolver(fakeResolver{"app.home.arpa": {"127.0.0.1"}})).GetJSON(context.Background(),
		mustURL(t, "http://app.home.arpa:"+port+"/"), nil, false, &out)
	require.NoError(t, err)
	assert.Equal(t, true, out["direct"])
	assert.Zero(t, proxyHits.Load())
}

// AC-019 — an error never carries the URL, a header or upstream text, so a
// log line or an admin response built from it can't leak them.
func TestClient_ErrorsCarryNoURLOrHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/sso/login?next=secret-path", http.StatusFound)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	var out map[string]any
	errs := []error{
		NewClient().GetJSON(context.Background(), mustURL(t, srv.URL+"/secret-path"), http.Header{"X-Api-Key": {"sk-live-abc"}}, false, &out),
		NewClient().GetJSON(context.Background(), mustURL(t, "http://169.254.169.254/secret-path"), http.Header{"X-Api-Key": {"sk-live-abc"}}, false, &out),
		NewClient(WithResolver(fakeResolver{})).GetJSON(context.Background(), mustURL(t, "http://missing.home.arpa/secret-path"), nil, false, &out),
	}
	for _, err := range errs {
		require.Error(t, err)
		msg := err.Error()
		for _, leak := range []string{host, "secret-path", "sk-live-abc", "169.254", "missing.home.arpa"} {
			assert.NotContains(t, msg, leak)
		}
		var we *Error
		assert.True(t, errors.As(err, &we), "every client error is a typed widget error")
	}
}
