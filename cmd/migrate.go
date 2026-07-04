package cmd

import (
	"context"
	"log/slog"
	"os"

	"github.com/parsa-poorsistani/sms-gw/configs"
	"github.com/parsa-poorsistani/sms-gw/internal/database"
	"github.com/spf13/cobra"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply database migrations",
	RunE:  runMigrate,
}

func runMigrate(_ *cobra.Command, _ []string) error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := configs.Load()
	if err != nil {
		log.Error("load config", "err", err)
		return err
	}

	ctx := context.Background()
	db, err := database.NewPostgresConnection(ctx, &cfg.Postgres, log)
	if err != nil {
		log.Error("connect to database", "err", err)
		return err
	}
	defer db.Close()

	return database.RunMigrations(ctx, db, cfg.Postgres.MigrationsPath, log)
}
