package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"xnc/server/internal/config"
)

type Store struct {
	pool *pgxpool.Pool
}

func OpenStore(ctx context.Context, cfg config.Config) (*Store, error) {
	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	pc.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) Close() { s.pool.Close() }
