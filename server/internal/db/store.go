package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"xnc/server/internal/config"
	"xnc/server/internal/db/sqlc"
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

// Q 返回基于连接池的 sqlc 查询对象。
func (s *Store) Q() *sqlc.Queries { return sqlc.New(s.pool) }

func (s *Store) Close() { s.pool.Close() }
