package storage

import (
	"context"
	"errors"
)

type Store struct {
	DSN string
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("storage.Open: DATABASE_URL is empty")
	}
	return &Store{DSN: dsn}, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	return errors.New("storage.Migrate: not implemented")
}

func (s *Store) Close() error { return nil }
