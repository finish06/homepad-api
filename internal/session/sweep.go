package session

import (
	"context"
	"time"
)

// RunSweeper deletes expired sessions once at start and then every `every`,
// until ctx is cancelled; it returns once the loop has stopped, so a caller can
// wait for it before closing the database. Expired rows are already ignored by
// Lookup — the sweep only keeps the table from growing. Errors are logged and
// the loop carries on.
func RunSweeper(ctx context.Context, st Store, every time.Duration, logf func(format string, args ...any)) {
	sweep := func() {
		n, err := st.DeleteExpired(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logf("session sweep: %v", err)
			}
			return
		}
		if n > 0 {
			logf("session sweep: deleted %d expired sessions", n)
		}
	}

	sweep()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
