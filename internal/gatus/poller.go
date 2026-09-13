package gatus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	StatusUp       = "UP"
	StatusDown     = "DOWN"
	StatusDegraded = "DEGRADED"
	StatusUnknown  = "UNKNOWN"
	// StatusNotMonitored is homepad-api's own sentinel for a service with no
	// gatus_key (monitoring never wired). Gatus never produces it.
	StatusNotMonitored = "NOT_MONITORED"
)

// CheckResult is a single historical Gatus check, preserved so the frontend can
// render an uptime sparkline. Oldest-first within EndpointStatus.Results.
type CheckResult struct {
	Success   bool
	Timestamp time.Time
	// Duration is the check's response time as Gatus measured it (its JSON
	// `duration`, nanoseconds). Zero when the payload omitted it — read that
	// as unknown, never as "0 ms" (SPEC-tile-density OQ-6).
	Duration time.Duration
}

type EndpointStatus struct {
	Key          string
	Status       string
	LastResultAt time.Time
	// Results is the recent check history (≤20, oldest-first) for the sparkline.
	// Empty when Gatus has no results for the endpoint.
	Results []CheckResult
	// Uptime is Gatus's own computed availability per long window (see
	// UptimeWindows), fraction 0..1. Only windows Gatus answered are present;
	// nil/empty when none. Backs the per-tile long-window uptime metrics.
	Uptime map[string]float64
}

// maxResults caps the surfaced history per endpoint, matching Gatus's default
// retention and the sparkline's 20-dot strip.
const maxResults = 20

// UptimeWindows are the long rolling windows surfaced per tile. They are a subset
// of Gatus's own supported durations (1h/24h/7d/30d); 1h is dropped because it
// overlaps the sparkline's short window.
var UptimeWindows = []string{"24h", "7d", "30d"}

type Snapshot struct {
	AsOf     time.Time
	Statuses map[string]EndpointStatus
}

// DefaultDegradedAfter is the response-time threshold past which a SUCCESSFUL
// check reads DEGRADED (Caleb, 2026-09-13: "succeeded but > 1000 ms"). Gatus has
// no degraded state of its own — a failed [RESPONSE_TIME] condition just fails
// the check — so homepad derives it from the duration Gatus reports. Overridden
// per install via GATUS_DEGRADED_MS; 0 disables the derivation.
const DefaultDegradedAfter = time.Second

type Client struct {
	BaseURL string
	// DegradedAfter — a succeeded check slower than this is DEGRADED. Strictly
	// greater than; 0 disables. See DefaultDegradedAfter. Guarded by mu because
	// the admin System panel changes it at runtime while the poller is running:
	// read via degradedAfter(), write via SetDegradedAfter.
	DegradedAfter time.Duration
	mu            sync.RWMutex
	http          *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{BaseURL: baseURL, DegradedAfter: DefaultDegradedAfter, http: &http.Client{}}
}

// SetDegradedAfter changes the "Slow" threshold for every poll from now on
// (runtime System setting). 0 disables the derivation.
func (c *Client) SetDegradedAfter(d time.Duration) {
	c.mu.Lock()
	c.DegradedAfter = d
	c.mu.Unlock()
}

func (c *Client) degradedAfter() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.DegradedAfter
}

// FetchAll pulls the full endpoint snapshot from Gatus. Status is derived from
// each endpoint's most recent result: success -> UP, failure -> DOWN, and no
// results -> UNKNOWN.
func (c *Client) FetchAll(ctx context.Context) ([]EndpointStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/endpoints/statuses", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var endpoints []struct {
		Key     string `json:"key"`
		Results []struct {
			Success   bool      `json:"success"`
			Timestamp time.Time `json:"timestamp"`
			Duration  int64     `json:"duration"` // nanoseconds; absent → 0
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&endpoints); err != nil {
		return nil, err
	}

	out := make([]EndpointStatus, 0, len(endpoints))
	for _, e := range endpoints {
		es := EndpointStatus{Key: e.Key, Status: StatusUnknown}
		if n := len(e.Results); n > 0 {
			last := e.Results[n-1]
			es.LastResultAt = last.Timestamp
			switch {
			case !last.Success:
				es.Status = StatusDown
			case c.degradedAfter() > 0 && last.Duration > 0 && time.Duration(last.Duration) > c.degradedAfter():
				// Answered, but slowly: the tile says "Slow", the panel goes amber.
				es.Status = StatusDegraded
			default:
				es.Status = StatusUp
			}
			// Surface the recent history for the sparkline. Gatus returns
			// results oldest-first (the last entry is the current check, used
			// for Status above), so keep that order; take the most recent 20.
			start := 0
			if n > maxResults {
				start = n - maxResults
			}
			es.Results = make([]CheckResult, 0, n-start)
			for _, r := range e.Results[start:] {
				es.Results = append(es.Results, CheckResult{
					Success:   r.Success,
					Timestamp: r.Timestamp,
					Duration:  time.Duration(r.Duration),
				})
			}
		}
		out = append(out, es)
	}
	return out, nil
}

// FetchUptime reads Gatus's own computed availability for one endpoint over one
// window from GET /api/v1/endpoints/{key}/uptimes/{window}, which returns a bare
// fraction (0..1) as text/plain (e.g. "0.945815"). homepad never recomputes this
// from raw history. A non-200 (404 unknown key, 400 bad window) is an error so the
// caller can omit the window rather than record 0.
func (c *Client) FetchUptime(ctx context.Context, key, window string) (float64, error) {
	url := c.BaseURL + "/api/v1/endpoints/" + key + "/uptimes/" + window
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("gatus uptime %s/%s: unexpected status %d", key, window, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(body)), 64)
}

// fillUptime layers Gatus's computed long-window uptime onto each endpoint, in
// place. These are extra GETs (the statuses payload carries no uptime), so it
// bounds concurrency and stays best-effort — a failed/404 window is simply
// omitted and never fails the poll.
func (c *Client) fillUptime(ctx context.Context, statuses []EndpointStatus) {
	const workers = 8
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range statuses {
		for _, win := range UptimeWindows {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, win string) {
				defer wg.Done()
				defer func() { <-sem }()
				v, err := c.FetchUptime(ctx, statuses[i].Key, win)
				if err != nil {
					return
				}
				mu.Lock()
				if statuses[i].Uptime == nil {
					statuses[i].Uptime = map[string]float64{}
				}
				statuses[i].Uptime[win] = v
				mu.Unlock()
			}(i, win)
		}
	}
	wg.Wait()
}

type Poller struct {
	client   *Client
	interval time.Duration

	mu       sync.RWMutex
	snapshot Snapshot
}

func NewPoller(client *Client, interval time.Duration) *Poller {
	return &Poller{
		client:   client,
		interval: interval,
		// Seed AsOf so the published snapshot always carries a timestamp, even
		// before the first poll completes (A4: status responses expose staleness).
		snapshot: Snapshot{AsOf: time.Now(), Statuses: map[string]EndpointStatus{}},
	}
}

func (p *Poller) Interval() time.Duration { return p.interval }

// SetDegradedAfter forwards the runtime "Slow" threshold to the client.
func (p *Poller) SetDegradedAfter(d time.Duration) { p.client.SetDegradedAfter(d) }

// DegradedAfter reports the threshold currently in force.
func (p *Poller) DegradedAfter() time.Duration { return p.client.degradedAfter() }

// Run polls Gatus immediately, then on each interval tick, until ctx is done.
// Transport errors are swallowed (A9): a failed poll leaves the published
// snapshot empty/stale rather than crashing the poller.
func (p *Poller) Run(ctx context.Context) error {
	p.poll(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			p.poll(ctx)
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	// Scheduled polls keep the A9 contract: a transport error publishes an
	// empty snapshot (every keyed service reads UNKNOWN) rather than crashing.
	snap, _ := p.fetch(ctx)
	p.publish(snap)
}

// fetch performs one poll and builds the snapshot it would publish. On a
// transport error the returned snapshot is empty and err is non-nil; the
// caller decides whether that empty snapshot replaces the last good one.
func (p *Poller) fetch(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	snap := Snapshot{AsOf: time.Now(), Statuses: map[string]EndpointStatus{}}
	statuses, err := p.client.FetchAll(ctx)
	if err != nil {
		return snap, err
	}
	// Best-effort: layer Gatus's own computed long-window uptime onto each
	// endpoint before publishing the snapshot.
	p.client.fillUptime(ctx, statuses)
	for _, st := range statuses {
		snap.Statuses[st.Key] = st
	}
	return snap, nil
}

func (p *Poller) publish(snap Snapshot) {
	p.mu.Lock()
	p.snapshot = snap
	p.mu.Unlock()
}

// PollNow re-polls Gatus synchronously on request — the primitive behind
// POST /api/status/refresh (SPEC-v24 §12.3, OQ-5: "Retry now" prods the
// poller instead of the client refetching the same stale payload).
//
// Unlike the scheduled poll, a failed manual re-poll does NOT replace the last
// good snapshot: it returns the error and leaves AsOf where it was. That is
// what lets the endpoint tell "could not reach Gatus" (error, old as_of) apart
// from "re-polled, still old" (no error, new as_of, same data).
func (p *Poller) PollNow(ctx context.Context) (Snapshot, error) {
	snap, err := p.fetch(ctx)
	if err != nil {
		return p.Snapshot(), err
	}
	p.publish(snap)
	return snap, nil
}

func (p *Poller) Snapshot() Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.snapshot
}
