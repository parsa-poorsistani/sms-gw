-- 001_init.sql
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE users (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    balance     BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE credit_transactions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id),
    amount      BIGINT NOT NULL,               -- positive = inc/refund, negative = deduction
    kind        TEXT NOT NULL CHECK (kind IN ('inc', 'deduct', 'refund')),
    message_id  UUID,                          -- set for deduct/refund
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_credit_tx_user ON credit_transactions (user_id, created_at DESC);

CREATE TABLE messages (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id),
    phone       TEXT NOT NULL,
    body        TEXT NOT NULL,
    express     BOOLEAN NOT NULL DEFAULT FALSE,
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'sending', 'sent', 'failed')),
    attempts    INT NOT NULL DEFAULT 0,
    provider_id TEXT,                          -- operator-side message id
    error       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at     TIMESTAMPTZ,
    -- Claim lease: stamped when a dispatcher worker claims the row. A row in
    -- 'sending' whose lease is older than the claim timeout can only belong
    -- to a dead worker, so the janitor safely returns it to 'pending'.
    claimed_at  TIMESTAMPTZ
);

CREATE INDEX idx_messages_pending ON messages (express DESC, created_at) WHERE status = 'pending';
CREATE INDEX idx_messages_user ON messages (user_id, created_at DESC);

CREATE INDEX idx_messages_sending ON messages (claimed_at) WHERE status = 'sending';
