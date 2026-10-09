package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolProbe reports PostgreSQL availability through a pool ping.
type PoolProbe struct {
	pool *pgxpool.Pool
}

// NewPoolProbe wraps an existing pool for health probing.
func NewPoolProbe(pool *pgxpool.Pool) *PoolProbe {
	return &PoolProbe{pool: pool}
}

// Check pings the pool, honoring the caller's deadline and cancellation.
func (p *PoolProbe) Check(ctx context.Context) error {
	return p.pool.Ping(ctx)
}
