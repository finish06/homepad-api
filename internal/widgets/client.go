package widgets

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// RED stub — implemented in the GREEN commit.

const RequestTimeout = 5 * time.Second
const MaxBodyBytes = 1 << 20

var ErrTooLarge = errors.New("widget: response too large")
var errBlockedDial = errors.New("widget: blocked address")

type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

type Option func(*Client)

func WithResolver(r Resolver) Option { return func(*Client) {} }
func WithDialer(d func(ctx context.Context, network, addr string) (net.Conn, error)) Option {
	return func(*Client) {}
}
func WithTimeout(d time.Duration) Option { return func(*Client) {} }

type Client struct{}

func NewClient(opts ...Option) *Client { return &Client{} }

type Request struct {
	Method        string
	URL           *url.URL
	Header        http.Header
	Body          []byte
	InsecureTLS   bool
	MaxBody       int64
	AllowTruncate bool
}

type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (c *Client) Do(ctx context.Context, req Request) (Response, error) {
	return Response{}, ErrUnreachable
}

func (c *Client) GetJSON(ctx context.Context, u *url.URL, h http.Header, insecureTLS bool, dst any) error {
	return ErrUnreachable
}

func IsBlockedIP(ip net.IP) bool { return false }

func controlCheck(network, address string, _ syscall.RawConn) error { return nil }
