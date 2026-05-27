package session

import "sync"

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]string
}

func NewManager() *Manager {
	return &Manager{sessions: map[string]string{}}
}
