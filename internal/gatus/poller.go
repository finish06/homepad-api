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

type EndpointStatus struct {
	Key          string
	Status       string
	LastResultAt time.Time
}

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
