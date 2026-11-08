package widgets

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Manager fetches every enabled service widget on its own interval and holds
// the latest values in memory (SPEC-status-widgets §6.3, AC-016). Handlers
// read it with View, which never waits on an upstream app, so GET
// /api/services can't fail or stall because of one.
//
//   - On start it loads every enabled widget and fetches each once, up to 4 at
//     a time, then each on its own interval.
//   - At most one fetch per widget is in flight; a Test connection run waits
//     for a scheduled fetch of the same widget instead of overlapping it.
//   - Each refresh has a 10s budget; each request inside it has 5s (Client).
//   - After 2 consecutive failures the next attempt waits 2× the interval,
//     doubling up to 15 min. A success or a config change resets it.
//   - Each fetch re-reads the widget's row, so a deleted service (FK cascade),
//     a disabled widget or a changed tile URL takes effect on the next fetch.
//   - The secret is decrypted just before the fetch and dropped after it.
//   - Logs name the widget and the error code only (AC-019).
type Manager struct {
	src    Source
	reg    *Registry
	client *Client
	sealer *Sealer
	now    func() time.Time
	logf   func(string, ...any)
	tick   time.Duration
	sem    chan struct{}
	wake   chan struct{}

	mu      sync.Mutex
	entries map[string]*entry
	locks   map[string]*sync.Mutex // one per widget: scheduled fetch vs Test
}

// Widget states, as the dashboard reads them (spec §6.3, AC-012).
const (
	StateLoading = "loading" // configured, no fetch finished since start or the last config change
	StateOK      = "ok"      // the last fetch succeeded, at most 2× the interval ago
	StateStale   = "stale"   // a good value under 30 min old, but the last fetch failed or is overdue
	StateError   = "error"   // no good value in 30 min
)

const (
	// RefreshBudget bounds one widget refresh, all of its requests together.
	RefreshBudget = 10 * time.Second
	// ErrorAfter is how long a widget may go without a good value before it
	// reads error and the tile falls back to its status line.
	ErrorAfter = 30 * time.Minute
	// MaxBackoff caps the wait between attempts of a failing widget.
	MaxBackoff = 15 * time.Minute
	// Parallel is how many widgets are fetched at once.
	Parallel = 4
)

// Stored is a widget's saved config, as the store returns it. Secret fields
// are the sealed ciphertext; the plaintext exists only during a fetch.
type Stored struct {
	ServiceID        string
	Kind             string
	Address          string // "" = use ServiceURL
	ServiceURL       string
	Username         string
	SecretCiphertext []byte
	SecretNonce      []byte
	Fields           []string
	Interval         time.Duration
	InsecureTLS      bool
	Enabled          bool
}

// EffectiveAddress is the address the widget fetches: its own, or the tile's
// link when blank.
func (s Stored) EffectiveAddress() string {
	if strings.TrimSpace(s.Address) != "" {
		return strings.TrimSpace(s.Address)
	}
	return strings.TrimSpace(s.ServiceURL)
}

// SecretAAD is the GCM additional data for this widget's key: the service id
// and the origin the key was entered for. A key sealed for one widget or one
// origin can't be opened for another (§6.4, AC-023).
func (s Stored) SecretAAD() []byte {
	return SecretAAD(s.ServiceID, s.EffectiveAddress())
}

// SecretAAD builds the additional data for a key sealed for serviceID at
// address.
func SecretAAD(serviceID, address string) []byte {
	return []byte("homepad-widget-key\x00" + serviceID + "\x00" + Origin(address))
}

// Origin is scheme://host:port, lower-cased, with the default port made
// explicit, or "" if raw isn't an absolute http(s) URL. Two addresses with the
// same origin reach the same server, so a saved key may stay.
func Origin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	switch {
	case scheme == "http" && port == "":
		port = "80"
	case scheme == "https" && port == "":
		port = "443"
	case scheme != "http" && scheme != "https":
		return ""
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// Source is where the manager reads widget configs.
type Source interface {
	EnabledWidgets(ctx context.Context) ([]Stored, error)
	Widget(ctx context.Context, serviceID string) (Stored, bool, error)
}

// ManagerOptions configures a Manager. Now, Logf and Tick have defaults.
type ManagerOptions struct {
	Source   Source
	Registry *Registry
	Client   *Client
	Sealer   *Sealer
	Now      func() time.Time
	Logf     func(format string, args ...any)
	Tick     time.Duration // how often Run checks for due widgets; default 1s
}

// FieldValue is one chosen field with its value, in the provider's order.
type FieldValue struct {
	Key   string
	Label string
	Unit  Unit
	Value float64
	Total *float64
}

// View is what a handler reads for one widget.
type View struct {
	Kind      string
	State     string
	AsOf      *time.Time // the last good fetch
	LastCheck *time.Time // the last finished fetch, good or not
	LastError Code       // the last fetch's code, "" when it succeeded
	Fields    []FieldValue
}

type entry struct {
	cfg      Stored
	st       *State
	inflight bool
	due      time.Time
	fails    int
	gen      int

	checked   bool
	lastCheck time.Time
	lastErr   Code
	hasGood   bool
	lastOK    time.Time
	good      []FieldValue
}

// NewManager builds a manager. Call LoadAll (Run does) before reading views.
func NewManager(o ManagerOptions) *Manager {
	m := &Manager{
		src: o.Source, reg: o.Registry, client: o.Client, sealer: o.Sealer,
		now: o.Now, logf: o.Logf, tick: o.Tick,
		sem:     make(chan struct{}, Parallel),
		wake:    make(chan struct{}, 1),
		entries: map[string]*entry{},
		locks:   map[string]*sync.Mutex{},
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.logf == nil {
		m.logf = func(string, ...any) {}
	}
	if m.tick <= 0 {
		m.tick = time.Second
	}
	if m.client == nil {
		m.client = NewClient()
	}
	return m
}

// Run loads every enabled widget, then fetches due widgets until ctx ends.
func (m *Manager) Run(ctx context.Context) error {
	if err := m.LoadAll(ctx); err != nil {
		m.logf("widgets: load failed; retrying on the next tick")
	}
	t := time.NewTicker(m.tick)
	defer t.Stop()
	loaded := true
	for {
		if loaded {
			m.RefreshDue(ctx)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-m.wake:
		}
		if !loaded {
			loaded = m.LoadAll(ctx) == nil
		}
	}
}

// LoadAll replaces the widget set with every enabled widget in the store.
// Each one starts in `loading` and is due at once.
func (m *Manager) LoadAll(ctx context.Context) error {
	rows, err := m.src.EnabledWidgets(ctx)
	if err != nil {
		return err
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	next := map[string]*entry{}
	for _, r := range rows {
		e := &entry{cfg: r, st: &State{}, due: now}
		if old, ok := m.entries[r.ServiceID]; ok {
			e.gen = old.gen + 1
		}
		next[r.ServiceID] = e
	}
	m.entries = next
	return nil
}

// Reload re-reads one widget after a config write: its state is dropped and
// it is fetched at once (a disabled or deleted widget is removed).
func (m *Manager) Reload(ctx context.Context, serviceID string) error {
	r, ok, err := m.src.Widget(ctx, serviceID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	old := m.entries[serviceID]
	if !ok || !r.Enabled {
		delete(m.entries, serviceID)
		m.mu.Unlock()
		return nil
	}
	e := &entry{cfg: r, st: &State{}, due: m.now()}
	if old != nil {
		e.gen = old.gen + 1
		e.inflight = old.inflight
	}
	m.entries[serviceID] = e
	m.mu.Unlock()
	m.poke()
	return nil
}

// Remove drops a widget (DELETE).
func (m *Manager) Remove(serviceID string) {
	m.mu.Lock()
	delete(m.entries, serviceID)
	m.mu.Unlock()
}

func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// RefreshDue fetches every due widget that isn't already in flight, at most
// Parallel at a time, and waits for them.
func (m *Manager) RefreshDue(ctx context.Context) {
	now := m.now()
	var due []string
	m.mu.Lock()
	for id, e := range m.entries {
		if !e.inflight && !now.Before(e.due) {
			e.inflight = true
			due = append(due, id)
		}
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, id := range due {
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			m.clearInflight(id)
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-m.sem }()
			m.fetch(ctx, id)
		}(id)
	}
	wg.Wait()
}

// RefreshNow fetches one widget now, ignoring its schedule (used after a
// config write by tests; Run uses Reload's wake-up instead).
func (m *Manager) RefreshNow(ctx context.Context, serviceID string) {
	m.mu.Lock()
	e, ok := m.entries[serviceID]
	if !ok || e.inflight {
		m.mu.Unlock()
		return
	}
	e.inflight = true
	m.mu.Unlock()
	m.fetch(ctx, serviceID)
}

func (m *Manager) clearInflight(id string) {
	m.mu.Lock()
	if e, ok := m.entries[id]; ok {
		e.inflight = false
	}
	m.mu.Unlock()
}

func (m *Manager) lockFor(serviceID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[serviceID]
	if !ok {
		l = &sync.Mutex{}
		m.locks[serviceID] = l
	}
	return l
}

// fetch runs one scheduled refresh of serviceID. The caller has set inflight.
func (m *Manager) fetch(ctx context.Context, serviceID string) {
	l := m.lockFor(serviceID)
	l.Lock()
	defer l.Unlock()

	m.mu.Lock()
	e, ok := m.entries[serviceID]
	if !ok {
		m.mu.Unlock()
		return
	}
	gen, st := e.gen, e.st
	m.mu.Unlock()

	// Re-read the row: the source of truth for address, key and fields.
	cfg, found, err := m.src.Widget(ctx, serviceID)
	if err != nil {
		m.finish(serviceID, gen, nil, CodeUnreachable, false)
		return
	}
	if !found || !cfg.Enabled {
		m.mu.Lock()
		if cur, ok := m.entries[serviceID]; ok && cur.gen == gen {
			delete(m.entries, serviceID)
		}
		m.mu.Unlock()
		return
	}

	fields, _, code := m.fetchOnce(ctx, cfg, st, fetchScheduled, "", nil)
	m.finish(serviceID, gen, fields, code, true)
	if code != "" {
		m.logf("widget %s (%s): %s", serviceID, cfg.Kind, code)
	}
}

type fetchMode int

const (
	fetchScheduled fetchMode = iota // the stored key, the stored address
	fetchTest                       // Test connection: the form's key and address, plus the version
)

// fetchOnce performs one provider fetch for cfg and returns the chosen fields
// in provider order and the version (tests only), or a code. A scheduled
// fetch decrypts the stored key just before the call; a test uses the
// plaintext key and base URL the handler resolved from the form.
func (m *Manager) fetchOnce(ctx context.Context, cfg Stored, st *State, mode fetchMode, testSecret string, base *url.URL) ([]FieldValue, string, Code) {
	p, ok := m.reg.Get(cfg.Kind)
	if !ok {
		return nil, "", CodeBadResponse
	}
	test := mode == fetchTest
	secret := testSecret
	if !test && cfg.SecretCiphertext != nil {
		plain, err := m.sealer.Open(cfg.SecretCiphertext, cfg.SecretNonce, cfg.SecretAAD())
		if err != nil {
			return nil, "", CodeSecretUnreadable
		}
		secret = string(plain)
	}
	if base == nil {
		u, err := url.Parse(cfg.EffectiveAddress())
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, "", CodeBlockedAddress
		}
		base = u
	}
	fields := cfg.Fields
	if len(fields) == 0 && test {
		fields = DefaultFields(p)
	}
	ctx, cancel := context.WithTimeout(ctx, RefreshBudget)
	defer cancel()
	res, err := p.Fetch(ctx, m.client, Config{
		BaseURL: base, Username: cfg.Username, Secret: secret, InsecureTLS: cfg.InsecureTLS,
		Fields: fields, WantVersion: test,
	}, st)
	if err != nil {
		return nil, "", CodeOf(err)
	}
	return chosen(p, fields, res), cleanVersion(res.Version), ""
}

// cleanVersion keeps an upstream version string only if it looks like one
// (at most 40 of [0-9A-Za-z.+_-]); anything else becomes "". It is the one
// piece of upstream text a Test connection returns (AC-019).
func cleanVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 40 {
		return ""
	}
	for _, r := range v {
		ok := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			r == '.' || r == '+' || r == '_' || r == '-'
		if !ok {
			return ""
		}
	}
	return v
}

// chosen lists the chosen fields the app answered, in provider order.
func chosen(p Provider, keys []string, res Result) []FieldValue {
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	out := []FieldValue{}
	for _, f := range p.Fields() {
		if !want[f.Key] {
			continue
		}
		v, ok := res.Values[f.Key]
		if !ok || v.Num == nil {
			continue
		}
		fv := FieldValue{Key: f.Key, Label: f.Label, Unit: f.Unit, Value: *v.Num}
		if v.Total != nil {
			t := *v.Total
			fv.Total = &t
		}
		out = append(out, fv)
	}
	return out
}

func (m *Manager) finish(serviceID string, gen int, fields []FieldValue, code Code, checked bool) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[serviceID]
	if !ok {
		return
	}
	if e.gen != gen {
		// The config changed during the fetch: its result is for the old
		// config. The new entry stays loading and is due now.
		e.inflight = false
		return
	}
	e.inflight = false
	if checked {
		e.checked = true
		e.lastCheck = now
	}
	if code == "" {
		e.fails = 0
		e.lastErr = ""
		e.hasGood = true
		e.lastOK = now
		e.good = fields
	} else {
		e.fails++
		e.lastErr = code
	}
	e.due = now.Add(nextDelay(e.fails, e.cfg.Interval))
}

// nextDelay is the wait before the next attempt (AC-020): the interval, or
// after 2 consecutive failures 2× it, doubling, capped at MaxBackoff.
func nextDelay(fails int, interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = time.Minute
	}
	if fails < 2 {
		return interval
	}
	d := interval
	for i := 1; i < fails; i++ {
		d *= 2
		if d >= MaxBackoff {
			return max(MaxBackoff, interval)
		}
	}
	return d
}

// View returns a widget's current state, computed at read time.
func (m *Manager) View(serviceID string) (View, bool) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[serviceID]
	if !ok {
		return View{}, false
	}
	v := View{Kind: e.cfg.Kind, LastError: e.lastErr, Fields: []FieldValue{}}
	if e.checked {
		t := e.lastCheck
		v.LastCheck = &t
	}
	if e.hasGood {
		t := e.lastOK
		v.AsOf = &t
	}
	switch {
	case e.hasGood && now.Sub(e.lastOK) <= ErrorAfter:
		v.Fields = append(v.Fields, e.good...)
		interval := e.cfg.Interval
		if interval <= 0 {
			interval = time.Minute
		}
		if e.lastErr == "" && now.Sub(e.lastOK) <= 2*interval {
			v.State = StateOK
		} else {
			v.State = StateStale
		}
	case !e.checked:
		v.State = StateLoading
	default:
		v.State = StateError
	}
	return v, true
}

// TestInput is a Test connection run's unsaved form values. Secret is the
// plaintext key to use ("" = none); the handler resolves "use the saved key".
type TestInput struct {
	Kind        string
	BaseURL     *url.URL
	Username    string
	Secret      string
	InsecureTLS bool
	Fields      []string
}

var errUnknownKind = errors.New("widget: unknown kind")

// Test runs one fetch with unsaved values (AC-021). It ignores the backoff,
// asks for the app version, never changes the dashboard state, and waits for
// any scheduled fetch of the same widget to finish first.
func (m *Manager) Test(ctx context.Context, serviceID string, in TestInput) ([]FieldValue, string, error) {
	if _, ok := m.reg.Get(in.Kind); !ok {
		return nil, "", errUnknownKind
	}
	l := m.lockFor(serviceID)
	l.Lock()
	defer l.Unlock()
	cfg := Stored{ServiceID: serviceID, Kind: in.Kind, Username: in.Username, Fields: in.Fields, InsecureTLS: in.InsecureTLS}
	fields, version, code := m.fetchOnce(ctx, cfg, &State{}, fetchTest, in.Secret, in.BaseURL)
	if code != "" {
		return nil, "", &Error{Code: code}
	}
	return fields, version, nil
}
