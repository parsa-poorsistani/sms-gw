package cmd

import (
	"context"
	"fmt"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/parsa-poorsistani/sms-gw/configs"
	"github.com/parsa-poorsistani/sms-gw/internal/api"
	"github.com/parsa-poorsistani/sms-gw/internal/database"
	"github.com/parsa-poorsistani/sms-gw/internal/dispatch"
	"github.com/parsa-poorsistani/sms-gw/internal/metrics"
	"github.com/parsa-poorsistani/sms-gw/internal/provider"
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the HTTP server and message dispatcher",
	RunE:  runServe,
}

func runServe(cmd *cobra.Command, _ []string) error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := configs.Load()
	if err != nil {
		log.Error("load config", "err", err)
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := database.NewPostgresConnection(ctx, &cfg.Postgres, log)
	if err != nil {
		log.Error("connect to database", "err", err)
		return err
	}
	defer db.Close()

	if err := database.RunMigrations(ctx, db, cfg.Postgres.MigrationsPath, log); err != nil {
		log.Error("run migrations", "err", err)
		return err
	}

	repo := database.NewRepository(db)

	schedule, err := provider.ParseSchedule(cfg.Provider.LatencySchedule)
	if err != nil {
		return fmt.Errorf("provider latency schedule: %w", err)
	}
	prov := &provider.Mock{
		Latency:     cfg.Provider.Latency,
		FailureRate: cfg.Provider.FailureRate,
		Schedule:    schedule,
	}
	if len(schedule) > 0 {
		log.Info("provider latency schedule active", "schedule", cfg.Provider.LatencySchedule)
	}

	d := dispatch.New(cfg.Dispatcher, repo, prov, log)
	d.Run(ctx)

	mux := http.NewServeMux()
	if cfg.Metrics.Enabled {
		mux.Handle(cfg.Metrics.Path, metrics.Handler())
	}
	mux.Handle("/", api.New(repo, log).Routes())

	srv := &http.Server{
		Addr:              cfg.Server.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
	}

	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()

	log.Info("sms-gateway listening", "addr", cfg.Server.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		return err
	}

	// HTTP has stopped; now drain the dispatcher so in-flight deliveries are
	// recorded instead of stranded in 'sending'. Bounded by the shutdown
	// timeout — anything still unresolved after that is the janitor's job.
	log.Info("draining dispatcher workers")
	drained := make(chan struct{})
	go func() {
		d.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		log.Info("dispatcher drained cleanly")
	case <-time.After(cfg.Server.ShutdownTimeout):
		log.Warn("dispatcher drain timed out; janitor will requeue stragglers")
	}
	return nil
}
