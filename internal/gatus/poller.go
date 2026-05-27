package gatus

import (
	"context"
	"errors"
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
}

func NewClient(baseURL string) *Client {
	return &Client{BaseURL: baseURL}
}

func (c *Client) FetchAll(ctx context.Context) ([]EndpointStatus, error) {
	return nil, errors.New("gatus.Client.FetchAll: not implemented")
}

type Poller struct {
	client   *Client
	interval time.Duration
}

func NewPoller(client *Client, interval time.Duration) *Poller {
	return &Poller{client: client, interval: interval}
}

func (p *Poller) Interval() time.Duration { return p.interval }

func (p *Poller) Run(ctx context.Context) error {
	return errors.New("gatus.Poller.Run: not implemented")
}

func (p *Poller) Snapshot() Snapshot {
	return Snapshot{
		AsOf:     time.Time{},
		Statuses: map[string]EndpointStatus{},
	}
}
