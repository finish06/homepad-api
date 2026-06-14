package gatus

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const (
	StatusUp       = "UP"
	StatusDown     = "DOWN"
	StatusDegraded = "DEGRADED"
	StatusUnknown  = "UNKNOWN"
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
}

// maxResults caps the surfaced history per endpoint, matching Gatus's default
// retention and the sparkline's 20-dot strip.
const maxResults = 20

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
