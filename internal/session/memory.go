package session

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-process Store for tests. Like PgStore it keys sessions
// by sha256(token) and never holds the raw token. Nothing survives a restart,
// so production uses PgStore.
type MemoryStore struct {
	// Clock is the time source; nil means time.Now. Tests inject a fake.
	Clock func() time.Time

	mu       sync.Mutex
	sessions map[[32]byte]Session
}

var _ Store = (*MemoryStore)(nil)

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{sessions: map[[32]byte]Session{}}
}

func (m *MemoryStore) Create(ctx context.Context, userID string, lifetime time.Duration, userAgent string) (string, Session, error) {
	if err := checkLifetime(lifetime); err != nil {
		return "", Session{}, err
	}
	token, err := newToken()
	if err != nil {
		return "", Session{}, err
	}
	s, err := m.insert(token, userID, lifetime, userAgent)
	return token, s, err
}

// Bind attaches a caller-chosen token to userID. It exists for seeding known
// tokens in integration-test fixtures; real sign-ins use Create.
func (m *MemoryStore) Bind(ctx context.Context, token, userID string, lifetime time.Duration) error {
	if err := checkLifetime(lifetime); err != nil {
		return err
	}
	_, err := m.insert(token, userID, lifetime, "")
	return err
}

func (m *MemoryStore) insert(token, userID string, lifetime time.Duration, userAgent string) (Session, error) {
	handle, err := newHandle()
	if err != nil {
		return Session{}, err
	}
	now := clockNow(m.Clock)
	s := Session{
		Handle:     handle,
		UserID:     userID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(lifetime),
		UserAgent:  sanitizeUserAgent(userAgent),
	}
	m.mu.Lock()
	m.sessions[hashToken(token)] = s
	m.mu.Unlock()
	return s, nil
}

func (m *MemoryStore) Lookup(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrNotFound
	}
	now := clockNow(m.Clock)
	m.mu.Lock()
	s, ok := m.sessions[hashToken(token)]
	m.mu.Unlock()
	if !ok || !now.Before(s.ExpiresAt) {
		return Session{}, ErrNotFound
	}
	s.TouchDue = touchDue(s.LastSeenAt, now)
	return s, nil
}

func (m *MemoryStore) Touch(ctx context.Context, token string, lifetime time.Duration) (bool, error) {
	if err := checkLifetime(lifetime); err != nil {
		return false, err
	}
	now := clockNow(m.Clock)
	h := hashToken(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[h]
	if !ok || !now.Before(s.ExpiresAt) || !touchDue(s.LastSeenAt, now) {
		return false, nil
	}
	s.LastSeenAt = now
	s.ExpiresAt = now.Add(lifetime)
	m.sessions[h] = s
	return true, nil
}

func (m *MemoryStore) Destroy(ctx context.Context, token string) error {
	m.mu.Lock()
	delete(m.sessions, hashToken(token))
	m.mu.Unlock()
	return nil
}

func (m *MemoryStore) ListForUser(ctx context.Context, userID string) ([]Session, error) {
	now := clockNow(m.Clock)
	m.mu.Lock()
	var out []Session
	for _, s := range m.sessions {
		if s.UserID == userID && now.Before(s.ExpiresAt) {
			s.TouchDue = touchDue(s.LastSeenAt, now)
			out = append(out, s)
		}
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return lessRecent(out[i], out[j]) })
	return out, nil
}

// lessRecent orders most recently seen first; ties by newest, then handle, so
// the order is deterministic. It mirrors PgStore's ORDER BY.
func lessRecent(a, b Session) bool {
	if !a.LastSeenAt.Equal(b.LastSeenAt) {
		return a.LastSeenAt.After(b.LastSeenAt)
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.Handle < b.Handle
}

func (m *MemoryStore) RevokeByHandle(ctx context.Context, userID, handle string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, s := range m.sessions {
		if s.Handle == handle && s.UserID == userID {
			delete(m.sessions, h)
			return nil
		}
	}
	return ErrNotFound
}

func (m *MemoryStore) DeleteExpired(ctx context.Context) (int64, error) {
	now := clockNow(m.Clock)
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for h, s := range m.sessions {
		if !now.Before(s.ExpiresAt) {
			delete(m.sessions, h)
			n++
		}
	}
	return n, nil
}
