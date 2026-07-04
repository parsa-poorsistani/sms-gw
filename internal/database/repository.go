package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/parsa-poorsistani/sms-gw/internal/metrics"
	"github.com/parsa-poorsistani/sms-gw/internal/store"
)

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) Close() error {
	return r.db.Close()
}

func (r *Repository) Ping(ctx context.Context) error {
	return r.db.PingContext(ctx)
}

func rejected(err error) bool {
	return errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInsufficientBalance)
}


func (r *Repository) CreateUser(ctx context.Context, name string) (u *store.User, err error) {
	defer metrics.ObserveRepo("create_user", &err, rejected, time.Now())

	u = &store.User{}
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO users (name) VALUES ($1) RETURNING id, name, balance, created_at`,
		name).Scan(&u.ID, &u.Name, &u.Balance, &u.CreatedAt)
	return u, err
}

func (r *Repository) GetUser(ctx context.Context, id uuid.UUID) (u *store.User, err error) {
	defer metrics.ObserveRepo("get_user", &err, rejected, time.Now())

	u = &store.User{}
	err = r.db.QueryRowContext(ctx,
		`SELECT id, name, balance, created_at FROM users WHERE id = $1`,
		id).Scan(&u.ID, &u.Name, &u.Balance, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return u, err
}

func (r *Repository) IncBalance(ctx context.Context, userID uuid.UUID, amount int64) (u *store.User, err error) {
	defer metrics.ObserveRepo("inc_balance", &err, rejected, time.Now())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	u = &store.User{}
	err = tx.QueryRowContext(ctx,
		`UPDATE users SET balance = balance + $2 WHERE id = $1
		 RETURNING id, name, balance, created_at`,
		userID, amount).Scan(&u.ID, &u.Name, &u.Balance, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO credit_transactions (user_id, amount, kind) VALUES ($1, $2, 'inc')`,
		userID, amount); err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

func (r *Repository) EnqueueMessage(ctx context.Context, userID uuid.UUID, phone, body string, express bool) (m *store.Message, err error) {
	defer metrics.ObserveRepo("enqueue_message", &err, rejected, time.Now())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	const cost = 1

	res, err := tx.ExecContext(ctx,
		`UPDATE users SET balance = balance - $2 WHERE id = $1 AND balance >= $2`,
		userID, cost)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		var exists bool
		if err = tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, store.ErrNotFound
		}
		return nil, store.ErrInsufficientBalance
	}

	m = &store.Message{}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO messages (user_id, phone, body, express)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, user_id, phone, body, express, status, attempts, created_at`,
		userID, phone, body, express).
		Scan(&m.ID, &m.UserID, &m.Phone, &m.Body, &m.Express, &m.Status, &m.Attempts, &m.CreatedAt)
	if err != nil {
		return nil, err
	}

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO credit_transactions (user_id, amount, kind, message_id)
		 VALUES ($1, $2, 'deduct', $3)`,
		userID, -cost, m.ID); err != nil {
		return nil, err
	}
	return m, tx.Commit()
}

func (r *Repository) ClaimPending(ctx context.Context, express bool, limit int) (out []store.Message, err error) {
	defer metrics.ObserveRepo("claim_pending", &err, rejected, time.Now())

	rows, err := r.db.QueryContext(ctx, `
		UPDATE messages SET status = 'sending', attempts = attempts + 1, claimed_at = now()
		WHERE id IN (
			SELECT id FROM messages
			WHERE status = 'pending' AND express = $1
			ORDER BY created_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, user_id, phone, body, express, status, attempts, created_at`,
		express, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var m store.Message
		if err = rows.Scan(&m.ID, &m.UserID, &m.Phone, &m.Body, &m.Express,
			&m.Status, &m.Attempts, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *Repository) MarkSent(ctx context.Context, id uuid.UUID, providerID string) (err error) {
	defer metrics.ObserveRepo("mark_sent", &err, rejected, time.Now())

	_, err = r.db.ExecContext(ctx,
		`UPDATE messages SET status = 'sent', provider_id = $2, sent_at = now(), error = NULL
		 WHERE id = $1`, id, providerID)
	return err
}

func (r *Repository) MarkForRetry(ctx context.Context, id uuid.UUID, reason string) (err error) {
	defer metrics.ObserveRepo("mark_for_retry", &err, rejected, time.Now())

	_, err = r.db.ExecContext(ctx,
		`UPDATE messages SET status = 'pending', error = $2 WHERE id = $1`, id, reason)
	return err
}

func (r *Repository) MarkFailed(ctx context.Context, id uuid.UUID, reason string) (err error) {
	defer metrics.ObserveRepo("mark_failed", &err, rejected, time.Now())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var userID uuid.UUID
	err = tx.QueryRowContext(ctx,
		`UPDATE messages SET status = 'failed', error = $2 WHERE id = $1 RETURNING user_id`,
		id, reason).Scan(&userID)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE users SET balance = balance + 1 WHERE id = $1`, userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO credit_transactions (user_id, amount, kind, message_id)
		 VALUES ($1, 1, 'refund', $2)`, userID, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) RequeueStale(ctx context.Context, olderThan time.Duration) (n int64, err error) {
	defer metrics.ObserveRepo("requeue_stale", &err, rejected, time.Now())

	res, err := r.db.ExecContext(ctx, `
		UPDATE messages
		SET status = 'pending', error = 'requeued: stale claim (worker died?)'
		WHERE status = 'sending' AND claimed_at < now() - make_interval(secs => $1)`,
		olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *Repository) ListMessages(ctx context.Context, userID uuid.UUID, before time.Time, limit int) (out []store.Message, err error) {
	defer metrics.ObserveRepo("list_messages", &err, rejected, time.Now())

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, user_id, phone, body, express, status, attempts,
		       provider_id, error, created_at, sent_at
		FROM messages
		WHERE user_id = $1 AND created_at < $2
		ORDER BY created_at DESC
		LIMIT $3`, userID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out = []store.Message{}
	for rows.Next() {
		var m store.Message
		if err = rows.Scan(&m.ID, &m.UserID, &m.Phone, &m.Body, &m.Express, &m.Status,
			&m.Attempts, &m.ProviderID, &m.Error, &m.CreatedAt, &m.SentAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
