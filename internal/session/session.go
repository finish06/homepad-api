package session

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
)

// Manager is an in-memory session store: opaque random token -> user id.
// v1 runs a single replica, so in-memory is sufficient (see spec deployment
// contract). A restart logs everyone out, which is acceptable for v1.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]string
}

func NewManager() *Manager {
	return &Manager{sessions: map[string]string{}}
}

// Create mints a new session token bound to userID.
func (m *Manager) Create(userID string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)

	m.mu.Lock()
	m.sessions[token] = userID
	m.mu.Unlock()
	return token, nil
}

// Bind attaches a caller-supplied token to userID. Create is preferred for real
// logins; Bind exists for seeding known tokens (e.g. integration-test fixtures).
func (m *Manager) Bind(token, userID string) {
	m.mu.Lock()
	m.sessions[token] = userID
	m.mu.Unlock()
}

// UserID returns the user bound to token, or ("", false) if unknown.
func (m *Manager) UserID(token string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.sessions[token]
	return id, ok
}

func (m *Manager) Destroy(token string) {
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
}
