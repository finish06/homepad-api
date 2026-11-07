package widgets

import (
	"context"
	"net/url"
	"time"
)

// RED stub — implemented in the GREEN commit.

const (
	StateLoading = "loading"
	StateOK      = "ok"
	StateStale   = "stale"
	StateError   = "error"
)

const RefreshBudget = 10 * time.Second

type Stored struct {
	ServiceID        string
	Kind             string
	Address          string
	ServiceURL       string
	Username         string
	SecretCiphertext []byte
	SecretNonce      []byte
	Fields           []string
	Interval         time.Duration
	InsecureTLS      bool
	Enabled          bool
}

func (s Stored) EffectiveAddress() string { return "" }
func (s Stored) SecretAAD() []byte        { return nil }

func Origin(raw string) string { return "" }

type Source interface {
	EnabledWidgets(ctx context.Context) ([]Stored, error)
	Widget(ctx context.Context, serviceID string) (Stored, bool, error)
}

type ManagerOptions struct {
	Source   Source
	Registry *Registry
	Client   *Client
	Sealer   *Sealer
	Now      func() time.Time
	Logf     func(format string, args ...any)
	Tick     time.Duration
}

type FieldValue struct {
	Key   string
	Label string
	Unit  Unit
	Value float64
	Total *float64
}

type View struct {
	Kind      string
	State     string
	AsOf      *time.Time
	LastCheck *time.Time
	LastError Code
	Fields    []FieldValue
}

type TestInput struct {
	Kind        string
	BaseURL     *url.URL
	Username    string
	Secret      string
	InsecureTLS bool
	Fields      []string
}

type Manager struct{}

func NewManager(o ManagerOptions) *Manager                            { return &Manager{} }
func (m *Manager) Run(ctx context.Context) error                      { return nil }
func (m *Manager) LoadAll(ctx context.Context) error                  { return nil }
func (m *Manager) Reload(ctx context.Context, serviceID string) error { return nil }
func (m *Manager) Remove(serviceID string)                            {}
func (m *Manager) RefreshNow(ctx context.Context, serviceID string)   {}
func (m *Manager) RefreshDue(ctx context.Context)                     {}
func (m *Manager) View(serviceID string) (View, bool)                 { return View{}, false }
func (m *Manager) Test(ctx context.Context, serviceID string, in TestInput) ([]FieldValue, string, error) {
	return nil, "", ErrUnreachable
}

func nextDelay(fails int, interval time.Duration) time.Duration { return 0 }
