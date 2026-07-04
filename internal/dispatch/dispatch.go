package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/parsa-poorsistani/sms-gw/configs"
	"github.com/parsa-poorsistani/sms-gw/internal/database"
	"github.com/parsa-poorsistani/sms-gw/internal/metrics"
	"github.com/parsa-poorsistani/sms-gw/internal/provider"
	"github.com/parsa-poorsistani/sms-gw/internal/store"
)

type Dispatcher struct {
	cfg  configs.DispatcherConfig
	repo *database.Repository
	prov provider.Provider
	log  *slog.Logger
	wg   sync.WaitGroup
}

func New(cfg configs.DispatcherConfig, repo *database.Repository, prov provider.Provider, log *slog.Logger) *Dispatcher {
	return &Dispatcher{cfg: cfg, repo: repo, prov: prov, log: log}
}

func (d *Dispatcher) Run(ctx context.Context) {
	for i := 0; i < d.cfg.ExpressWorkers; i++ {
		d.wg.Add(1)
		go d.worker(ctx, true, d.cfg.ExpressBatchSize)
	}
	for i := 0; i < d.cfg.StandardWorkers; i++ {
		d.wg.Add(1)
		go d.worker(ctx, false, d.cfg.BatchSize)
	}
	d.wg.Add(1)
	go d.janitor(ctx)
}

func (d *Dispatcher) Wait() {
	d.wg.Wait()
}

func (d *Dispatcher) worker(ctx context.Context, express bool, batchSize int) {
	defer d.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := d.repo.ClaimPending(ctx, express, batchSize)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.log.Error("claim failed", "err", err)
			d.sleep(ctx)
			continue
		}
		if len(msgs) == 0 {
			d.sleep(ctx)
			continue
		}
		for _, m := range msgs {
			d.deliver(ctx, m)
		}
	}
}

func (d *Dispatcher) sleep(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(d.cfg.PollInterval):
	}
}

func (d *Dispatcher) deliver(ctx context.Context, m store.Message) {
	sctx, cancel := context.WithTimeout(ctx, d.cfg.SendTimeout)
	defer cancel()

	providerID, err := d.prov.Send(sctx, m.Phone, m.Body)

	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer fcancel()

	switch {
	case err == nil:
		if err = d.repo.MarkSent(fctx, m.ID, providerID); err != nil {
			d.log.Error("mark sent failed", "msg", m.ID, "err", err)
		} else {
			metrics.RecordMessageOutcome("sent")
		}

	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		if err = d.repo.MarkForRetry(fctx, m.ID, "requeued: shutdown during send"); err != nil {
			d.log.Error("shutdown requeue failed", "msg", m.ID, "err", err)
		} else {
			metrics.RecordMessageOutcome("requeued_shutdown")
		}

	case isTransient(err) && m.Attempts < d.cfg.MaxAttempts:
		if err = d.repo.MarkForRetry(fctx, m.ID, err.Error()); err != nil {
			d.log.Error("mark retry failed", "msg", m.ID, "err", err)
		} else {
			metrics.RecordMessageOutcome("retry")
		}

	default:
		if err = d.repo.MarkFailed(fctx, m.ID, err.Error()); err != nil {
			d.log.Error("mark failed failed", "msg", m.ID, "err", err)
		} else {
			metrics.RecordMessageOutcome("failed")
		}
	}
}

func (d *Dispatcher) janitor(ctx context.Context) {
	defer d.wg.Done()
	ticker := time.NewTicker(d.cfg.JanitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := d.repo.RequeueStale(ctx, d.cfg.ClaimTimeout)
			if err != nil {
				if ctx.Err() == nil {
					d.log.Error("janitor requeue failed", "err", err)
				}
				continue
			}
			if n > 0 {
				d.log.Warn("janitor requeued stale messages", "count", n)
				metrics.RecordRescued(n)
			}
		}
	}
}

func isTransient(err error) bool {
	var te *provider.TransientError
	return errors.As(err, &te) || errors.Is(err, context.DeadlineExceeded)
}
