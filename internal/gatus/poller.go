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

type Client struct {
	BaseURL string
	http    *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{BaseURL: baseURL, http: &http.Client{}}
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
			if last.Success {
				es.Status = StatusUp
			} else {
				es.Status = StatusDown
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
				es.Results = append(es.Results, CheckResult{Success: r.Success, Timestamp: r.Timestamp})
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
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	snap := Snapshot{AsOf: time.Now(), Statuses: map[string]EndpointStatus{}}
	if statuses, err := p.client.FetchAll(ctx); err == nil {
		// Best-effort: layer Gatus's own computed long-window uptime onto each
		// endpoint before publishing the snapshot.
		p.client.fillUptime(ctx, statuses)
		for _, st := range statuses {
			snap.Statuses[st.Key] = st
		}
	}

	p.mu.Lock()
	p.snapshot = snap
	p.mu.Unlock()
}

func (p *Poller) Snapshot() Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.snapshot
}
