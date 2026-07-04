package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/parsa-poorsistani/sms-gw/configs"

	_ "github.com/lib/pq"
)

func NewPostgresConnection(ctx context.Context, cfg *configs.PostgresConfig, log *slog.Logger) (*sql.DB, error) {
	if cfg == nil {
		return nil, fmt.Errorf("postgres config is required")
	}

	db, err := sql.Open("postgres", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConnections)
	db.SetMaxIdleConns(cfg.MaxIdleConnections)
	if cfg.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	if log != nil {
		log.Info("database connected", "dbname", cfg.DBName, "host", cfg.Host, "port", cfg.Port)
	}
	return db, nil
}
