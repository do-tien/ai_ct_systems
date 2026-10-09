// Package postgres provides the PostgreSQL integration adapter.
package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Settings holds the connection settings for the PostgreSQL pool.
type Settings struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
}

// NewPool builds a connection pool from explicit settings. The base config
// carries no credentials; SSL is disabled because the backend and PostgreSQL
// communicate over the local Docker network only. The pool connects lazily, so
// the backend can start listening while the database is still down.
func NewPool(ctx context.Context, settings Settings) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig("postgres://localhost:5432/postgres?sslmode=disable")
	if err != nil {
		return nil, err
	}
	poolConfig.ConnConfig.Host = settings.Host
	poolConfig.ConnConfig.Port = uint16(settings.Port)
	poolConfig.ConnConfig.Database = settings.Database
	poolConfig.ConnConfig.User = settings.User
	poolConfig.ConnConfig.Password = settings.Password
	poolConfig.MaxConns = 4
	return pgxpool.NewWithConfig(ctx, poolConfig)
}
