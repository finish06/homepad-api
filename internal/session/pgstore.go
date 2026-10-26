package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgStore keeps sessions in the Postgres `sessions` table (migration 0017), so
// they survive restarts and deploys (AC-010). It stores sha256(token) only
// (AC-011). Every timestamp comes from Clock, not the database's now(), so
// expiry and the slide throttle are decided by one clock and are testable.
type PgStore struct {
	// Clock is the time source; nil means time.Now. Tests inject a fake.
	Clock func() time.Time

	pool *pgxpool.Pool
}

var _ Store = (*PgStore)(nil)

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

const sessionColumns = `handle, user_id, created_at, last_seen_at, expires_at, user_agent`

func (p *PgStore) Create(ctx context.Context, userID string, lifetime time.Duration, userAgent string) (string, Session, error) {
	if err := checkLifetime(lifetime); err != nil {
		return "", Session{}, err
	}
	token, err := newToken()
	if err != nil {
		return "", Session{}, err
	}
	s, err := p.insert(ctx, token, userID, lifetime, userAgent)
	if err != nil {
		return "", Session{}, err
	}
	return token, s, nil
}

// Bind attaches a caller-chosen token to userID. It exists for seeding known
// tokens in integration-test fixtures; real sign-ins use Create.
func (p *PgStore) Bind(ctx context.Context, token, userID string, lifetime time.Duration) error {
	if err := checkLifetime(lifetime); err != nil {
		return err
	}
	_, err := p.insert(ctx, token, userID, lifetime, "")
	return err
}

func (p *PgStore) insert(ctx context.Context, token, userID string, lifetime time.Duration, userAgent string) (Session, error) {
	handle, err := newHandle()
	if err != nil {
		return Session{}, err
	}
	now := clockNow(p.Clock)
	h := hashToken(token)
	s := Session{
		Handle:     handle,
		UserID:     userID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(lifetime),
		UserAgent:  sanitizeUserAgent(userAgent),
	}
	if _, err := p.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, handle, user_id, created_at, last_seen_at, expires_at, user_agent)
		 VALUES ($1, $2, $3, $4, $4, $5, $6)`,
		h[:], s.Handle, s.UserID, now, s.ExpiresAt, s.UserAgent); err != nil {
		return Session{}, fmt.Errorf("session: create: %w", err)
	}
	return s, nil
}

func (p *PgStore) Lookup(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrNotFound
	}
	now := clockNow(p.Clock)
	h := hashToken(token)
	s, err := scanSession(p.pool.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE token_hash = $1 AND expires_at > $2`,
		h[:], now))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("session: lookup: %w", err)
	}
	s.TouchDue = touchDue(s.LastSeenAt, now)
	return s, nil
}

func (p *PgStore) Touch(ctx context.Context, token string, lifetime time.Duration) (bool, error) {
	if err := checkLifetime(lifetime); err != nil {
		return false, err
	}
	now := clockNow(p.Clock)
	h := hashToken(token)
	// One conditional UPDATE: the throttle and liveness checks live in the
	// WHERE clause, so concurrent Touches cannot both write.
	tag, err := p.pool.Exec(ctx,
		`UPDATE sessions SET last_seen_at = $2, expires_at = $3
		 WHERE token_hash = $1 AND expires_at > $2 AND last_seen_at <= $4`,
		h[:], now, now.Add(lifetime), now.Add(-TouchInterval))
	if err != nil {
		return false, fmt.Errorf("session: touch: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (p *PgStore) Destroy(ctx context.Context, token string) error {
	h := hashToken(token)
	if _, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, h[:]); err != nil {
		return fmt.Errorf("session: destroy: %w", err)
	}
	return nil
}

func (p *PgStore) ListForUser(ctx context.Context, userID string) ([]Session, error) {
	now := clockNow(p.Clock)
	rows, err := p.pool.Query(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE user_id = $1 AND expires_at > $2
		 ORDER BY last_seen_at DESC, created_at DESC, handle`,
		userID, now)
	if isInvalidText(err) {
		return nil, nil // not a user id at all, so no sessions
	}
	if err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("session: list: %w", err)
		}
		s.TouchDue = touchDue(s.LastSeenAt, now)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		if isInvalidText(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: list: %w", err)
	}
	return out, nil
}

func (p *PgStore) RevokeByHandle(ctx context.Context, userID, handle string) error {
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM sessions WHERE user_id = $1 AND handle = $2`, userID, handle)
	if isInvalidText(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("session: revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PgStore) DeleteExpired(ctx context.Context) (int64, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1`, clockNow(p.Clock))
	if err != nil {
		return 0, fmt.Errorf("session: delete expired: %w", err)
	}
	return tag.RowsAffected(), nil
}

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.Handle, &s.UserID, &s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt, &s.UserAgent)
	return s, err
}

// isInvalidText reports a Postgres "invalid input syntax" error — here, a user
// id that is not a UUID, which can name no session.
func isInvalidText(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
