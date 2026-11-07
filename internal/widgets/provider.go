// Package widgets fetches a few numbers from the apps behind catalog tiles
// (SPEC-status-widgets). The API server talks to the upstream app, so keys stay
// on the server and the browser never sees an internal address or a key.
//
// The package sits beside the Gatus poller and shares its shape, not its code:
// a Manager with Run(ctx) started from main.go, a snapshot read without
// blocking, and handlers that merge it into responses.
//
// Only typed numbers leave this package (AC-019). A provider returns a Result
// of Values keyed by field; nothing here returns, stores or logs a raw body.
package widgets

import (
	"context"
	"errors"
	"net/url"
	"sync"
)

// AuthType is how a provider signs in to its app.
type AuthType string

const (
	AuthNone         AuthType = "none"
	AuthAPIKey       AuthType = "api_key"
	AuthPassword     AuthType = "password"
	AuthUserPassword AuthType = "user_password"
	AuthToken        AuthType = "token"
)

// Unit tells the browser how to format a value (spec §6.6).
type Unit string

const (
	UnitCount       Unit = "count"
	UnitPercent     Unit = "percent"
	UnitBytesPerSec Unit = "bytes_per_sec"
	UnitRatio       Unit = "ratio" // running/total
)

// FieldSpec is one number a provider can return.
type FieldSpec struct {
	Key     string `json:"key"`     // stable id, e.g. "playing"
	Label   string `json:"label"`   // short copy, e.g. "playing" (UX §9)
	Unit    Unit   `json:"unit"`    // formatting hint
	Default bool   `json:"default"` // preselected for a new widget (at most 3)
}

// Config is what a provider needs for one fetch. Secret is decrypted just
// before Fetch and never logged.
type Config struct {
	BaseURL     *url.URL // the widget's address, or the tile URL when blank
	Username    string   // "" for every Phase 1 provider
	Secret      string
	InsecureTLS bool
	// Fields are the keys the admin chose. A provider may skip an endpoint
	// that serves none of them, so a widget showing two numbers makes only the
	// requests those two need.
	Fields []string
	// WantVersion asks for the app version too (Test connection only), so a
	// scheduled refresh doesn't spend a request on it.
	WantVersion bool
}

// Wants reports whether the admin chose any of keys (or chose nothing, which
// means "everything", e.g. a test with no fields ticked yet).
func (c Config) Wants(keys ...string) bool {
	if len(c.Fields) == 0 {
		return true
	}
	for _, f := range c.Fields {
		for _, k := range keys {
			if f == k {
				return true
			}
		}
	}
	return false
}

// Value is one typed number. Total is set only for Unit "ratio".
type Value struct {
	Num   *float64
	Total *float64
}

// Result is a provider's answer: typed values keyed by FieldSpec.Key. A field
// the app didn't answer is absent.
type Result struct {
	Values  map[string]Value
	Version string // for the Test connection summary, "" if unknown
}

// State is per-widget memory owned by the manager (a Phase 2 session id or
// cookie). It is dropped when the config changes.
type State struct {
	mu   sync.Mutex
	data map[string]string
}

// Get returns a stored value.
func (s *State) Get(k string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[k]
}

// Set stores a value.
func (s *State) Set(k, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string]string{}
	}
	s.data[k] = v
}

// Provider is one app type. Every Phase 2 provider is just a new
// implementation of this interface plus its field copy.
type Provider interface {
	Kind() string  // "jellyfin"
	Label() string // "Jellyfin"
	Auth() AuthType
	Fields() []FieldSpec // every field it can return, in display order
	Match() []string     // name/slug hints for AC-025, e.g. {"jellyfin"}
	Help() string        // where to find the key (UX §6.1)
	Fetch(ctx context.Context, c *Client, cfg Config, st *State) (Result, error)
}

// Registry is the source of truth for widget kinds. The DB stores the kind as
// text and the API validates it here, so a new provider needs no migration.
type Registry struct {
	list   []Provider
	byKind map[string]Provider
}

// NewRegistry builds a registry. A duplicate kind panics: it is a programming
// error caught by any test that builds the default registry.
func NewRegistry(ps ...Provider) *Registry {
	r := &Registry{byKind: map[string]Provider{}}
	for _, p := range ps {
		if _, dup := r.byKind[p.Kind()]; dup {
			panic("widgets: duplicate provider kind " + p.Kind())
		}
		r.byKind[p.Kind()] = p
		r.list = append(r.list, p)
	}
	return r
}

// Get returns the provider for kind.
func (r *Registry) Get(kind string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.byKind[kind]
	return p, ok
}

// All returns every provider in registration order.
func (r *Registry) All() []Provider {
	if r == nil {
		return nil
	}
	return append([]Provider(nil), r.list...)
}

// FieldSpecOf returns the spec for one of p's fields.
func FieldSpecOf(p Provider, key string) (FieldSpec, bool) {
	for _, f := range p.Fields() {
		if f.Key == key {
			return f, true
		}
	}
	return FieldSpec{}, false
}

// NormalizeFields de-duplicates keys and puts them in p's display order (W-2:
// the order on the tile is the provider's order). It returns the first key p
// doesn't know, if any.
func NormalizeFields(p Provider, keys []string) ([]string, string) {
	want := map[string]bool{}
	for _, k := range keys {
		if _, ok := FieldSpecOf(p, k); !ok {
			return nil, k
		}
		want[k] = true
	}
	out := make([]string, 0, len(want))
	for _, f := range p.Fields() {
		if want[f.Key] {
			out = append(out, f.Key)
		}
	}
	return out, ""
}

// DefaultFields returns p's preselected field keys.
func DefaultFields(p Provider) []string {
	var out []string
	for _, f := range p.Fields() {
		if f.Default {
			out = append(out, f.Key)
		}
	}
	return out
}

// Code is a widget error code (spec §7.4). Codes are shown to admins only, as
// fixed copy (UX §6.4); a dashboard tile never shows one (AC-013).
type Code string

const (
	CodeAuth               Code = "auth"
	CodeUnreachable        Code = "unreachable"
	CodeTimeout            Code = "timeout"
	CodeTLS                Code = "tls"
	CodeRedirect           Code = "redirect"
	CodeBlockedAddress     Code = "blocked_address"
	CodeBadResponse        Code = "bad_response"
	CodeUnsupportedVersion Code = "unsupported_version"
	CodeSecretUnreadable   Code = "secret_unreadable"
	CodeNoSecretKey        Code = "no_secret_key"
)

// AllCodes lists every code, for tests and docs.
var AllCodes = []Code{
	CodeAuth, CodeUnreachable, CodeTimeout, CodeTLS, CodeRedirect, CodeBlockedAddress,
	CodeBadResponse, CodeUnsupportedVersion, CodeSecretUnreadable, CodeNoSecretKey,
}

// Error is a typed widget failure. It carries only its code: no URL, no
// header, no upstream text, so logging or returning one can't leak anything
// (AC-019). errors.Is matches on the code.
type Error struct{ Code Code }

func (e *Error) Error() string { return "widget: " + string(e.Code) }

// Is makes errors.Is(err, ErrAuth) match any *Error with the same code.
func (e *Error) Is(target error) bool {
	var t *Error
	return errors.As(target, &t) && t.Code == e.Code
}

// Typed errors a provider returns. The client maps transport failures to the
// others.
var (
	ErrAuth               = &Error{Code: CodeAuth}
	ErrBadResponse        = &Error{Code: CodeBadResponse}
	ErrUnsupportedVersion = &Error{Code: CodeUnsupportedVersion}
	ErrUnreachable        = &Error{Code: CodeUnreachable}
	ErrTimeout            = &Error{Code: CodeTimeout}
	ErrTLS                = &Error{Code: CodeTLS}
	ErrRedirect           = &Error{Code: CodeRedirect}
	ErrBlockedAddress     = &Error{Code: CodeBlockedAddress}
	ErrSecretUnreadable   = &Error{Code: CodeSecretUnreadable}
	ErrNoSecretKey        = &Error{Code: CodeNoSecretKey}
)

// CodeOf maps any error to a code. An error that isn't a widget error (a bug,
// a context cancel) reads as unreachable: the admin sees "Can't reach", never
// the error text.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CodeTimeout
	}
	return CodeUnreachable
}
