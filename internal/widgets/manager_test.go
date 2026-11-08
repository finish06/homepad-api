package widgets

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-status-widgets §6.3, AC-012, AC-016, AC-020 — the manager schedules,
// caches and backs off. A fake clock drives every test.

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 7, 14, 0, 0, 0, time.UTC)}
}
func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeProvider struct {
	kind string

	mu        sync.Mutex
	calls     int
	err       error
	values    map[string]float64
	gotSecret string
	gotCfg    Config
	deadline  time.Duration

	inflight, maxInflight atomic.Int32
	block                 chan struct{}
	blockHost             string // when set, only fetches to this host block
	started               chan struct{}
}

func newFakeProvider(kind string) *fakeProvider {
	return &fakeProvider{kind: kind, values: map[string]float64{"a": 1, "b": 2, "c": 3, "d": 4}}
}

func (p *fakeProvider) Kind() string   { return p.kind }
func (p *fakeProvider) Label() string  { return "Fake" }
func (p *fakeProvider) Auth() AuthType { return AuthAPIKey }
func (p *fakeProvider) Fields() []FieldSpec {
	return []FieldSpec{
		{Key: "a", Label: "alpha", Unit: UnitCount, Default: true},
		{Key: "b", Label: "beta", Unit: UnitCount, Default: true},
		{Key: "c", Label: "gamma", Unit: UnitPercent},
		{Key: "d", Label: "delta", Unit: UnitRatio},
	}
}
func (p *fakeProvider) Match() []string { return []string{"fake"} }
func (p *fakeProvider) Help() string    { return "Settings → Keys." }
func (p *fakeProvider) Fetch(ctx context.Context, c *Client, cfg Config, st *State) (Result, error) {
	n := p.inflight.Add(1)
	defer p.inflight.Add(-1)
	for {
		m := p.maxInflight.Load()
		if n <= m || p.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	if p.started != nil {
		p.started <- struct{}{}
	}
	if p.block != nil && (p.blockHost == "" || (cfg.BaseURL != nil && cfg.BaseURL.Host == p.blockHost)) {
		<-p.block
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.gotSecret = cfg.Secret
	p.gotCfg = cfg
	if dl, ok := ctx.Deadline(); ok {
		p.deadline = time.Until(dl)
	}
	if p.err != nil {
		return Result{}, p.err
	}
	res := Result{Values: map[string]Value{}, Version: "1.2.3"}
	for k, v := range p.values {
		v := v
		val := Value{Num: &v}
		if k == "d" {
			tot := 7.0
			val.Total = &tot
		}
		res.Values[k] = val
	}
	return res, nil
}
func (p *fakeProvider) setErr(err error) { p.mu.Lock(); p.err = err; p.mu.Unlock() }
func (p *fakeProvider) callCount() int   { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

type fakeSource struct {
	mu   sync.Mutex
	rows map[string]Stored
}

func newFakeSource(rows ...Stored) *fakeSource {
	s := &fakeSource{rows: map[string]Stored{}}
	for _, r := range rows {
		s.rows[r.ServiceID] = r
	}
	return s
}
func (s *fakeSource) EnabledWidgets(context.Context) ([]Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Stored
	for _, r := range s.rows {
		if r.Enabled {
			out = append(out, r)
		}
	}
	return out, nil
}
func (s *fakeSource) Widget(_ context.Context, id string) (Stored, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	return r, ok, nil
}
func (s *fakeSource) put(r Stored)  { s.mu.Lock(); s.rows[r.ServiceID] = r; s.mu.Unlock() }
func (s *fakeSource) del(id string) { s.mu.Lock(); delete(s.rows, id); s.mu.Unlock() }

func row(id string) Stored {
	return Stored{
		ServiceID: id, Kind: "fake", ServiceURL: "http://app.home.arpa:8096",
		Fields: []string{"a", "c"}, Interval: time.Minute, Enabled: true,
	}
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) Logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}
func (l *logSink) all() string { l.mu.Lock(); defer l.mu.Unlock(); return strings.Join(l.lines, "\n") }

type rig struct {
	m     *Manager
	clock *fakeClock
	prov  *fakeProvider
	src   *fakeSource
	logs  *logSink
}

func newRig(t *testing.T, sealer *Sealer, rows ...Stored) rig {
	t.Helper()
	r := rig{clock: newFakeClock(), prov: newFakeProvider("fake"), src: newFakeSource(rows...), logs: &logSink{}}
	r.m = NewManager(ManagerOptions{
		Source:   r.src,
		Registry: NewRegistry(r.prov),
		Client:   NewClient(),
		Sealer:   sealer,
		Now:      r.clock.Now,
		Logf:     r.logs.Logf,
	})
	require.NoError(t, r.m.LoadAll(context.Background()))
	return r
}

func TestManager_LoadingThenOK_FieldsInChosenOrder(t *testing.T) {
	r := newRig(t, nil, row("s1"))
	v, ok := r.m.View("s1")
	require.True(t, ok)
	assert.Equal(t, StateLoading, v.State)
	assert.Empty(t, v.Fields)
	assert.Nil(t, v.AsOf)

	r.m.RefreshDue(context.Background())
	v, _ = r.m.View("s1")
	assert.Equal(t, StateOK, v.State)
	require.NotNil(t, v.AsOf)
	assert.Equal(t, r.clock.Now(), *v.AsOf)
	require.Len(t, v.Fields, 2, "only the chosen fields")
	assert.Equal(t, "a", v.Fields[0].Key)
	assert.Equal(t, "alpha", v.Fields[0].Label)
	assert.Equal(t, UnitCount, v.Fields[0].Unit)
	assert.Equal(t, 1.0, v.Fields[0].Value)
	assert.Equal(t, "c", v.Fields[1].Key)
	assert.Equal(t, []string{"a", "c"}, r.prov.gotCfg.Fields, "the provider is told which fields to fetch")
	assert.False(t, r.prov.gotCfg.WantVersion, "a scheduled refresh doesn't ask for the version")

	_, ok = r.m.View("nope")
	assert.False(t, ok, "no widget, no view")
}

func TestManager_RatioCarriesTotal(t *testing.T) {
	rw := row("s1")
	rw.Fields = []string{"d"}
	r := newRig(t, nil, rw)
	r.m.RefreshDue(context.Background())
	v, _ := r.m.View("s1")
	require.Len(t, v.Fields, 1)
	require.NotNil(t, v.Fields[0].Total)
	assert.Equal(t, 7.0, *v.Fields[0].Total)
}

func TestManager_StaleThenError(t *testing.T) {
	r := newRig(t, nil, row("s1"))
	ctx := context.Background()
	r.m.RefreshDue(ctx)

	// Overdue with no failure: older than 2× the interval → stale.
	r.clock.Advance(2*time.Minute + time.Second)
	v, _ := r.m.View("s1")
	assert.Equal(t, StateStale, v.State)
	assert.Len(t, v.Fields, 2, "stale keeps the last values")

	// A failed fetch right after a good one → stale at once, with the code.
	r2 := newRig(t, nil, row("s2"))
	r2.m.RefreshDue(ctx)
	r2.prov.setErr(ErrAuth)
	r2.clock.Advance(time.Minute)
	r2.m.RefreshDue(ctx)
	v, _ = r2.m.View("s2")
	assert.Equal(t, StateStale, v.State)
	assert.Equal(t, CodeAuth, v.LastError)
	assert.Len(t, v.Fields, 2)

	// 30 min with no good value → error, and no fields.
	r2.clock.Advance(29*time.Minute + time.Second)
	v, _ = r2.m.View("s2")
	assert.Equal(t, StateError, v.State)
	assert.Empty(t, v.Fields)
	assert.Equal(t, CodeAuth, v.LastError)
}

func TestManager_FirstFetchFailsIsError(t *testing.T) {
	r := newRig(t, nil, row("s1"))
	r.prov.setErr(ErrUnreachable)
	r.m.RefreshDue(context.Background())
	v, _ := r.m.View("s1")
	assert.Equal(t, StateError, v.State)
	assert.Equal(t, CodeUnreachable, v.LastError)
	assert.Empty(t, v.Fields)
	require.NotNil(t, v.LastCheck)
}

func TestManager_BackoffDelay(t *testing.T) {
	m := time.Minute
	assert.Equal(t, m, nextDelay(0, m))
	assert.Equal(t, m, nextDelay(1, m), "one failure keeps the interval")
	assert.Equal(t, 2*m, nextDelay(2, m), "after 2 failures, 2×")
	assert.Equal(t, 4*m, nextDelay(3, m), "doubling")
	assert.Equal(t, 8*m, nextDelay(4, m))
	assert.Equal(t, 15*m, nextDelay(5, m), "capped at 15 min")
	assert.Equal(t, 15*m, nextDelay(50, m))
	assert.Equal(t, 15*m, nextDelay(3, 15*m), "a 15 min widget stays at 15 min")
	assert.Equal(t, 10*m, nextDelay(2, 5*m))
}

func TestManager_BackoffScheduling(t *testing.T) {
	r := newRig(t, nil, row("s1"))
	ctx := context.Background()
	r.prov.setErr(ErrTimeout)
	r.m.RefreshDue(ctx) // failure 1
	r.clock.Advance(time.Minute)
	r.m.RefreshDue(ctx) // failure 2 → next waits 2 min
	require.Equal(t, 2, r.prov.callCount())

	r.clock.Advance(time.Minute)
	r.m.RefreshDue(ctx)
	assert.Equal(t, 2, r.prov.callCount(), "backing off: not due after one interval")
	r.clock.Advance(time.Minute)
	r.m.RefreshDue(ctx)
	assert.Equal(t, 3, r.prov.callCount(), "due after 2× the interval")

	// A success resets the backoff.
	r.prov.setErr(nil)
	r.clock.Advance(4 * time.Minute)
	r.m.RefreshDue(ctx)
	require.Equal(t, 4, r.prov.callCount())
	r.clock.Advance(time.Minute)
	r.m.RefreshDue(ctx)
	assert.Equal(t, 5, r.prov.callCount(), "back on the normal interval")
}

func TestManager_ReloadResetsStateAndBackoff(t *testing.T) {
	r := newRig(t, nil, row("s1"))
	ctx := context.Background()
	r.prov.setErr(ErrAuth)
	for i := 0; i < 4; i++ {
		r.m.RefreshDue(ctx)
		r.clock.Advance(16 * time.Minute)
	}
	calls := r.prov.callCount()

	r.prov.setErr(nil)
	rw := row("s1")
	rw.Fields = []string{"b"}
	r.src.put(rw)
	require.NoError(t, r.m.Reload(ctx, "s1"))
	v, _ := r.m.View("s1")
	assert.Equal(t, StateLoading, v.State, "a config change starts over")
	assert.Empty(t, v.LastError)

	r.m.RefreshDue(ctx)
	assert.Equal(t, calls+1, r.prov.callCount(), "due immediately, backoff reset")
	v, _ = r.m.View("s1")
	assert.Equal(t, StateOK, v.State)
	require.Len(t, v.Fields, 1)
	assert.Equal(t, "b", v.Fields[0].Key)
}

func TestManager_ReloadOfDisabledOrDeletedDrops(t *testing.T) {
	r := newRig(t, nil, row("s1"), row("s2"))
	ctx := context.Background()
	rw := row("s1")
	rw.Enabled = false
	r.src.put(rw)
	require.NoError(t, r.m.Reload(ctx, "s1"))
	_, ok := r.m.View("s1")
	assert.False(t, ok, "a disabled widget has no view")

	r.src.del("s2")
	require.NoError(t, r.m.Reload(ctx, "s2"))
	_, ok = r.m.View("s2")
	assert.False(t, ok)

	// A new widget appears on Reload.
	r.src.put(row("s3"))
	require.NoError(t, r.m.Reload(ctx, "s3"))
	v, ok := r.m.View("s3")
	require.True(t, ok)
	assert.Equal(t, StateLoading, v.State)

	r.m.Remove("s3")
	_, ok = r.m.View("s3")
	assert.False(t, ok)
}

// Each fetch re-reads the row, so a widget whose service was deleted (FK
// cascade) stops being fetched without any explicit call.
func TestManager_RowGoneIsDroppedOnNextFetch(t *testing.T) {
	r := newRig(t, nil, row("s1"))
	ctx := context.Background()
	r.m.RefreshDue(ctx)
	r.src.del("s1")
	r.clock.Advance(time.Minute)
	r.m.RefreshDue(ctx)
	assert.Equal(t, 1, r.prov.callCount())
	_, ok := r.m.View("s1")
	assert.False(t, ok)
}

func TestManager_OneInFlightPerWidget(t *testing.T) {
	r := newRig(t, nil, row("s1"))
	r.prov.block = make(chan struct{})
	r.prov.started = make(chan struct{}, 4)
	ctx := context.Background()

	done := make(chan struct{})
	go func() { r.m.RefreshNow(ctx, "s1"); close(done) }()
	<-r.prov.started

	// The scheduler skips a widget that is already being fetched.
	r.m.RefreshDue(ctx)

	// A test of the same widget waits for the scheduled fetch to finish.
	testDone := make(chan struct{})
	go func() {
		_, _, _ = r.m.Test(ctx, "s1", TestInput{Kind: "fake", BaseURL: mustURL(t, "http://app.home.arpa"), Fields: []string{"a"}})
		close(testDone)
	}()
	select {
	case <-testDone:
		t.Fatal("the test overlapped the scheduled fetch")
	case <-time.After(50 * time.Millisecond):
	}
	r.prov.block <- struct{}{}
	<-done
	<-r.prov.started
	r.prov.block <- struct{}{}
	<-testDone
	assert.Equal(t, int32(1), r.prov.maxInflight.Load())
	assert.Equal(t, 2, r.prov.callCount())
}

func TestManager_AtMostFourAtATime(t *testing.T) {
	var rows []Stored
	for i := 0; i < 7; i++ {
		rows = append(rows, row(fmt.Sprintf("s%d", i)))
	}
	r := newRig(t, nil, rows...)
	r.prov.block = make(chan struct{})
	r.prov.started = make(chan struct{}, 8)
	done := make(chan struct{})
	go func() { r.m.RefreshDue(context.Background()); close(done) }()
	for i := 0; i < 4; i++ {
		<-r.prov.started
	}
	select {
	case <-r.prov.started:
		t.Fatal("a fifth fetch started while four were in flight")
	case <-time.After(50 * time.Millisecond):
	}
	for i := 0; i < 7; i++ {
		if i >= 4 {
			<-r.prov.started
		}
		r.prov.block <- struct{}{}
	}
	<-done
	assert.LessOrEqual(t, r.prov.maxInflight.Load(), int32(4))
	assert.Equal(t, 7, r.prov.callCount())
}

func TestManager_RefreshBudgetIs10s(t *testing.T) {
	assert.Equal(t, 10*time.Second, RefreshBudget)
	r := newRig(t, nil, row("s1"))
	r.m.RefreshDue(context.Background())
	assert.LessOrEqual(t, r.prov.deadline, RefreshBudget)
	assert.Greater(t, r.prov.deadline, RefreshBudget-time.Second)
}

func TestManager_DecryptsJustBeforeFetch(t *testing.T) {
	a, _ := ParseSecretKey(testKeyA)
	b, _ := ParseSecretKey(testKeyB)
	rw := row("s1")
	ct, nonce, err := a.Seal([]byte("jf-key-123"), rw.SecretAAD())
	require.NoError(t, err)
	rw.SecretCiphertext, rw.SecretNonce = ct, nonce

	r := newRig(t, a, rw)
	r.m.RefreshDue(context.Background())
	assert.Equal(t, "jf-key-123", r.prov.gotSecret)
	assert.NotContains(t, r.logs.all(), "jf-key-123")

	// AC-005 — the server key changed: the widget errors with
	// secret_unreadable and its app is never called with a wrong key.
	r2 := newRig(t, b, rw)
	r2.m.RefreshDue(context.Background())
	v, _ := r2.m.View("s1")
	assert.Equal(t, StateError, v.State)
	assert.Equal(t, CodeSecretUnreadable, v.LastError)
	assert.Zero(t, r2.prov.callCount())

	// The server key was lost (unset): same.
	r3 := newRig(t, nil, rw)
	r3.m.RefreshDue(context.Background())
	v, _ = r3.m.View("s1")
	assert.Equal(t, CodeSecretUnreadable, v.LastError)
	assert.Zero(t, r3.prov.callCount())

	// The key is bound to the address it was entered for: a tile URL change
	// (blank widget address) makes the old key unreadable, so it is never sent
	// to the new host (AC-023).
	moved := rw
	moved.ServiceURL = "http://elsewhere.example.com"
	r4 := newRig(t, a, moved)
	r4.m.RefreshDue(context.Background())
	v, _ = r4.m.View("s1")
	assert.Equal(t, CodeSecretUnreadable, v.LastError)
	assert.Zero(t, r4.prov.callCount())
}

func TestManager_UnknownKindIsBadResponse(t *testing.T) {
	rw := row("s1")
	rw.Kind = "gone"
	r := newRig(t, nil, rw)
	r.m.RefreshDue(context.Background())
	v, ok := r.m.View("s1")
	require.True(t, ok)
	assert.Equal(t, StateError, v.State)
	assert.Equal(t, CodeBadResponse, v.LastError)
}

// AC-019 — logs name the widget and the error code, never the address, a
// header or the key.
func TestManager_LogsNameWidgetAndCodeOnly(t *testing.T) {
	a, _ := ParseSecretKey(testKeyA)
	rw := row("s1")
	rw.Address = "http://jellyfin.home.arpa:8096/secret-path"
	ct, nonce, _ := a.Seal([]byte("jf-key-123"), rw.SecretAAD())
	rw.SecretCiphertext, rw.SecretNonce = ct, nonce
	r := newRig(t, a, rw)
	r.prov.setErr(ErrAuth)
	r.m.RefreshDue(context.Background())

	logs := r.logs.all()
	assert.Contains(t, logs, "s1")
	assert.Contains(t, logs, "auth")
	for _, leak := range []string{"jellyfin.home.arpa", "secret-path", "jf-key-123", "8096"} {
		assert.NotContains(t, logs, leak)
	}
}

func TestManager_TestIgnoresBackoffAndAsksForVersion(t *testing.T) {
	r := newRig(t, nil, row("s1"))
	ctx := context.Background()
	r.prov.setErr(ErrAuth)
	for i := 0; i < 5; i++ {
		r.m.RefreshDue(ctx)
	}
	r.prov.setErr(nil)
	fields, version, err := r.m.Test(ctx, "s1", TestInput{
		Kind: "fake", BaseURL: mustURL(t, "http://app.home.arpa:8096"), Secret: "typed-key", Fields: []string{"b", "d"},
	})
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", version)
	require.Len(t, fields, 2)
	assert.Equal(t, "b", fields[0].Key)
	assert.Equal(t, "typed-key", r.prov.gotSecret)
	assert.True(t, r.prov.gotCfg.WantVersion)

	// A test failure is a code, never text.
	r.prov.setErr(ErrRedirect)
	_, _, err = r.m.Test(ctx, "s1", TestInput{Kind: "fake", BaseURL: mustURL(t, "http://app.home.arpa")})
	assert.Equal(t, CodeRedirect, CodeOf(err))

	_, _, err = r.m.Test(ctx, "s9", TestInput{Kind: "nope", BaseURL: mustURL(t, "http://app.home.arpa")})
	assert.Error(t, err, "an unknown kind can't be tested")

	// A test never changes the dashboard state.
	v, _ := r.m.View("s1")
	assert.Equal(t, StateError, v.State)
}

func TestManager_RunLoadsFetchesAndStops(t *testing.T) {
	clock := newFakeClock()
	prov := newFakeProvider("fake")
	m := NewManager(ManagerOptions{
		Source: newFakeSource(row("s1")), Registry: NewRegistry(prov), Client: NewClient(),
		Now: clock.Now, Tick: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- m.Run(ctx) }()
	require.Eventually(t, func() bool {
		v, ok := m.View("s1")
		return ok && v.State == StateOK
	}, 2*time.Second, 5*time.Millisecond)

	// Reload wakes the loop at once.
	m.Reload(ctx, "s1") //nolint:errcheck
	require.Eventually(t, func() bool { return prov.callCount() >= 2 }, 2*time.Second, 5*time.Millisecond)

	cancel()
	select {
	case err := <-errc:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestStored_AddressAndOrigin(t *testing.T) {
	rw := row("s1")
	assert.Equal(t, "http://app.home.arpa:8096", rw.EffectiveAddress(), "blank address = the tile URL")
	rw.Address = "https://Jellyfin.Home.Arpa/jf"
	assert.Equal(t, "https://Jellyfin.Home.Arpa/jf", rw.EffectiveAddress())

	assert.Equal(t, "https://jellyfin.home.arpa:443", Origin("https://Jellyfin.Home.Arpa/jf"))
	assert.Equal(t, "http://app.home.arpa:80", Origin("http://app.home.arpa"))
	assert.Equal(t, "http://app.home.arpa:8096", Origin("http://app.home.arpa:8096/x?y=1"))
	assert.Equal(t, "http://[fd12::1]:8080", Origin("http://[fd12::1]:8080/"))
	assert.Equal(t, "", Origin("not a url"))
	assert.Equal(t, Origin("https://a.example.com"), Origin("https://a.example.com:443/other"))
	assert.NotEqual(t, Origin("https://a.example.com"), Origin("http://a.example.com"))
}

// AC-019 — the version is the one piece of upstream text a test returns, so
// it is reduced to a short, plain version string.
func TestCleanVersion(t *testing.T) {
	assert.Equal(t, "10.10.3", cleanVersion("10.10.3"))
	assert.Equal(t, "4.0.11.2680-develop", cleanVersion(" 4.0.11.2680-develop\n"))
	assert.Equal(t, "", cleanVersion("<script>alert(1)</script>"))
	assert.Equal(t, "", cleanVersion("v1 with spaces"))
	assert.Len(t, cleanVersion(strings.Repeat("1", 500)), 0)
	assert.Equal(t, "1.2.3+build", cleanVersion("1.2.3+build"))
}

// AC-016 — a fetch stuck in its upstream never blocks what a handler calls:
// View (GET /api/services), Reload (PUT), Remove (DELETE), RefreshDue (the
// loop) and LoadAll. Only the per-widget fetch lock is held across the
// upstream call, and none of these take it.
func TestManager_HungFetchBlocksNoReader(t *testing.T) {
	other := row("s2")
	other.ServiceURL = "http://other.home.arpa:8989"
	r := newRig(t, nil, row("s1"), other)
	r.prov.block = make(chan struct{})
	r.prov.blockHost = "app.home.arpa:8096" // only s1's upstream hangs
	r.prov.started = make(chan struct{}, 8)
	ctx := context.Background()

	fetched := make(chan struct{})
	go func() { r.m.RefreshNow(ctx, "s1"); close(fetched) }()
	select {
	case <-r.prov.started: // the fetch is inside the provider, holding the upstream
	case <-time.After(2 * time.Second):
		t.Fatal("the fetch never reached the provider")
	}

	calls := map[string]func(){
		"View":    func() { r.m.View("s1") },
		"Reload":  func() { _ = r.m.Reload(ctx, "s2") },
		"Remove":  func() { r.m.Remove("nope") },
		"LoadAll": func() { _ = r.m.LoadAll(ctx) },
	}
	for name, call := range calls {
		returned := make(chan struct{})
		go func() { call(); close(returned) }()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatalf("%s waited on a fetch that is stuck in its upstream", name)
		}
	}
	v, ok := r.m.View("s1")
	require.True(t, ok)
	assert.Equal(t, StateLoading, v.State, "an in-flight first fetch reads as loading")

	// RefreshDue skips the in-flight widget instead of waiting for it (or
	// queueing a second fetch of it), and still fetches the others: s2 is
	// due again after LoadAll.
	before := r.prov.callCount()
	dueDone := make(chan struct{})
	go func() { r.m.RefreshDue(ctx); close(dueDone) }()
	select {
	case <-dueDone:
	case <-time.After(time.Second):
		t.Fatal("RefreshDue waited on the stuck fetch")
	}
	assert.Equal(t, before+1, r.prov.callCount(), "s2 was fetched; s1 was skipped, not queued")
	v, _ = r.m.View("s2")
	assert.Equal(t, StateOK, v.State)

	r.prov.block <- struct{}{}
	<-fetched
}
