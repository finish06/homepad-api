package api

import (
	"testing"
	"time"
)

// SPEC-household-basics §3.4 / AC-017 — the token bucket behind the public
// invite routes, on a fake clock.

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestInviteRateLimit_BurstThenRefill(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	b := newTokenBucket(RateLimit{PerMinute: 30, Burst: 10}, clock.now)

	for i := 0; i < 10; i++ {
		if ok, _ := b.take(); !ok {
			t.Fatalf("request %d is inside the burst of 10", i+1)
		}
	}
	ok, retry := b.take()
	if ok {
		t.Fatal("the 11th request in the same instant is over the limit")
	}
	if retry != 2*time.Second {
		t.Fatalf("retry after = %v, want 2s (one token every 2s at 30 a minute)", retry)
	}

	clock.t = clock.t.Add(1 * time.Second)
	if ok, retry := b.take(); ok || retry != time.Second {
		t.Fatalf("after 1s: ok=%v retry=%v, want refused with 1s left", ok, retry)
	}
	clock.t = clock.t.Add(1 * time.Second)
	if ok, _ := b.take(); !ok {
		t.Fatal("after 2s one token has refilled")
	}
	if ok, _ := b.take(); ok {
		t.Fatal("and only one")
	}

	// A long idle refills to the burst, never past it.
	clock.t = clock.t.Add(time.Hour)
	for i := 0; i < 10; i++ {
		if ok, _ := b.take(); !ok {
			t.Fatalf("after an idle hour, request %d is inside the burst", i+1)
		}
	}
	if ok, _ := b.take(); ok {
		t.Fatal("the bucket never holds more than the burst")
	}
}

func TestInviteRateLimit_DefaultIs30PerMinuteBurst10(t *testing.T) {
	if DefaultInviteLimit != (RateLimit{PerMinute: 30, Burst: 10}) {
		t.Fatalf("DefaultInviteLimit = %+v, want 30 a minute, burst 10", DefaultInviteLimit)
	}
	// The zero value means "use the default".
	b := newTokenBucket(RateLimit{}, time.Now)
	n := 0
	for {
		ok, _ := b.take()
		if !ok {
			break
		}
		n++
		if n > 100 {
			t.Fatal("a zero RateLimit must fall back to the default, not to no limit")
		}
	}
	if n != 10 {
		t.Fatalf("zero RateLimit allowed a burst of %d, want the default 10", n)
	}
}

func TestRetryAfterSeconds_RoundsUp(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Second:             "1",
		1500 * time.Millisecond: "2",
		2 * time.Second:         "2",
		time.Millisecond:        "1",
		0:                       "1",
	} {
		if got := retryAfterSeconds(d); got != want {
			t.Errorf("retryAfterSeconds(%v) = %q, want %q", d, got, want)
		}
	}
}
