package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const migrationsTable = "_schema_migrations"

// Arbitrary but fixed application-wide lock key. Any process running
// migrations against the same database serializes on this advisory lock,
// so concurrent `serve` replicas can't race each other applying the same
// migration (one would fail on e.g. CREATE TABLE already existing).
const migrationLockKey = 727272

func acquireMigrationLock(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey)
	return err
}

func releaseMigrationLock(conn *sql.Conn) {
	_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey)
}

type Migration struct {
	Version int
	Name    string
	SQL     string
}

func ensureMigrationsTable(ctx context.Context, db *sql.DB) error {
	query := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			version INTEGER PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			applied_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`, migrationsTable)
	_, err := db.ExecContext(ctx, query)
	return err
}

func RunMigrations(ctx context.Context, db *sql.DB, migrationsPath string, log *slog.Logger) error {
	// Advisory locks are session-scoped, so pin one connection for the
	// whole run; pool rotation would otherwise release the lock early.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	if err := acquireMigrationLock(ctx, conn); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer releaseMigrationLock(conn)

	if err := ensureMigrationsTable(ctx, db); err != nil {
		return fmt.Errorf("ensure migrations table: %w", err)
	}

	migrations, err := loadMigrations(migrationsPath, log)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}

	appliedVersions, err := getAppliedMigrations(ctx, db)
	if err != nil {
		return fmt.Errorf("get applied migrations: %w", err)
	}

	pendingCount := 0
	for _, m := range migrations {
		if appliedVersions[m.Version] {
			continue
		}
		pendingCount++

		if log != nil {
			log.Info("applying migration", "version", m.Version, "name", m.Name)
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", m.Version, m.Name, err)
		}
		if log != nil {
			log.Info("migration applied", "version", m.Version, "name", m.Name)
		}
	}

	if log != nil {
		if pendingCount == 0 {
			log.Info("database is up to date")
		} else {
			log.Info("migrations complete", "count", pendingCount)
		}
	}
	return nil
}

func loadMigrations(migrationsPath string, log *slog.Logger) ([]Migration, error) {
	entries, err := os.ReadDir(migrationsPath)
	if err != nil {
		return nil, fmt.Errorf("read migrations directory %q: %w", migrationsPath, err)
	}

	var migrations []Migration
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}

		version, migName, err := parseMigrationFilename(name)
		if err != nil {
			if log != nil {
				log.Warn("skipping invalid migration file", "filename", name, "err", err)
			}
			continue
		}

		content, err := os.ReadFile(filepath.Join(migrationsPath, name))
		if err != nil {
			return nil, fmt.Errorf("read migration file %s: %w", name, err)
		}

		migrations = append(migrations, Migration{
			Version: version,
			Name:    migName,
			SQL:     string(content),
		})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})
	return migrations, nil
}

func parseMigrationFilename(filename string) (version int, name string, err error) {
	filename = strings.TrimSuffix(filename, ".sql")

	parts := strings.SplitN(filename, "_", 2)
	if len(parts) < 2 {
		return 0, "", fmt.Errorf("filename must be VERSION_name.sql")
	}

	version, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, "", fmt.Errorf("invalid version number: %w", err)
	}
	return version, parts[1], nil
}

func getAppliedMigrations(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT version FROM %s ORDER BY version", migrationsTable))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = true
	}
	return applied, rows.Err()
}

func applyMigration(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("execute migration SQL: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("INSERT INTO %s (version, name) VALUES ($1, $2)", migrationsTable),
		m.Version, m.Name,
	); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	return tx.Commit()
}
