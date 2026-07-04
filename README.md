# SMS Gateway

A multi-tenant, prepaid SMS gateway in Go: race-free credit accounting, an
express delivery tier with a real latency guarantee, asynchronous dispatch to
the operator with retries/refunds/crash-recovery, delivery reports, and a
reproducible load-test suite that validates all of it with exact arithmetic.

Built for a system that serves tens of thousands of businesses sending
~100M messages/day (~1,200 msg/s average), with heavily skewed per-tenant
send rates.

- [Quick start](#quick-start)
- [API](#api)
- [Architecture](#architecture)
- [The three core mechanisms](#the-three-core-mechanisms)
- [Failure handling](#failure-handling)
- [Observability](#observability)
- [Configuration](#configuration)
- [Load testing & measured results](#load-testing--measured-results)
- [Scaling roadmap](#scaling-roadmap)
- [Repository layout](#repository-layout)

---

## Quick start

```bash
docker compose up --build          # API on http://localhost:8080

./deployment/run-minikube.sh       # builds image in-cluster, migrates, deploys, smoke-tests

./deployment/load/run-load-test.sh                      # 1,200 msg/s x 3m, drain + ledger checks
MODE=exhaustion FUND=2000 ./deployment/load/run-load-test.sh   # credit invariant under contention
MODE=spikes ./deployment/load/run-load-test.sh          # 8m production-shaped run (spikes + brownout)
MODE=knee   ./deployment/load/run-load-test.sh          # ramp 200 -> 6,000/s, find the knee
```

## API

All bodies JSON. No auth (per the challenge brief). Prices: 1 credit per
message, single-page, Persian and English priced identically.

| Endpoint | Purpose | Outcomes |
|---|---|---|
| `POST /users` `{"name":...}` | create user | `201` user |
| `GET /users/{id}` | user + balance | `200` / `404` |
| `POST /users/{id}/credit` `{"amount":N}` | top up | `200` / `400` (amount ≤ 0) |
| `POST /users/{id}/messages` `{"phone","body","express"}` | send (async) | `202` queued / `402` insufficient balance / `400` invalid |
| `GET /users/{id}/messages?limit&before` | delivery report, newest-first, keyset-paginated | `200` (`next_before` cursor when page full) |
| `GET /healthz` | liveness (process up; deliberately DB-independent) | `200` |
| `GET /readyz` | readiness (pings DB; unready pods leave the Service) | `200` / `503` |
| `GET /metrics` | Prometheus | `200` |

Message lifecycle: `pending → sending → sent` | `pending` (retry) |
`failed` (terminal, **credit refunded**).

## Architecture

```
 clients ──REST──▶ ┌─────────────────gateway process (N replicas)───────────────────────── ─┐
                   │                                                                        │
                   │  API layer (HTTP handlers)          dispatcher (goroutine pools)       │
                   │   │ one ACID tx: conditional         ├─ express workers (small batch)  │
                   │   │ decrement + INSERT pending       ├─ standard workers (large batch) │
                   │   │ + ledger row                     └─ janitor (lease expiry)         │
                   └───┼───────────────────────────────────────┬───────────────┬────────────┘
                       ▼                                       │ claim:        │ deliver
                  PostgreSQL ◀── append-only credit ledger ────┘ FOR UPDATE    ▼
                  (the only shared state; all coordination       SKIP LOCKED  operator
                   between replicas happens here)                             (SMPP/HTTP;
                                                                               mock here)
```

The API layer and the dispatcher are **the same process** — `serve` starts
the worker goroutines and then the HTTP server. The database sits
between replicas, which is also why the two halves *could* be split into
separate deployments later (a mode flag) without any protocol between them.

Two logical components in one binary (`serve` runs both; they can be split
into API-only / dispatcher-only deployments later without code changes.

- **API layer** — accepting a message is a single tx. Returns
  `202` (delivery is asynchronous) or `402`. Never talks to the operator.
- **Dispatcher** — worker pools that claim `pending` messages and push them
  to the operator, driving the status state machine. Ingest rate and delivery
  rate are fully decoupled: a slow operator deepens the queue, it cannot slow
  the API.

## The three core mechanisms

### 1. The credit invariant (no overspend, full spend)

The two hard requirements — a customer must be able to spend their *entire*
balance, and must *never* send after it reaches zero, including under
concurrent requests — are both enforced by a single statement:

```sql
UPDATE users SET balance = balance - 1 WHERE id = $1 AND balance >= 1;
```
The decrement, the message insert, and an append-only ledger row
(`credit_transactions`) commit in **one transaction** — there is no state
where money moved but no message exists, or vice versa. `balance ==
SUM(ledger)` per user is the reconciliation invariant, and the load-test
suite asserts it after every run.

**Measured:** 300 concurrent connections firing 200,000 sends at ~8,800 req/s
against 50 users funded with exactly 2,000 credits each → exactly 100,000
accepted, 100,000 rejected with 402, zero users over budget, zero residual
balances, ledger reconciled. (See [exhaustion run](#run-2-exhaustion).)

### 2. TX Atomicity (the queue)

The message row with `status='pending'` *is* the event, written 
in the same transaction as the charge — and here the
dispatcher consumes the outbox table directly, so the table is the queue.

Workers claim disjoint batches with:

```sql
UPDATE messages SET status='sending', attempts=attempts+1, claimed_at=now()
WHERE id IN (SELECT id FROM messages
             WHERE status='pending' AND express=$1
             ORDER BY created_at LIMIT $2
             FOR UPDATE SKIP LOCKED)
RETURNING ...;
```

Known costs, owned explicitly: MVCC churn, polling latency (`LISTEN/NOTIFY` is 
the designed optimization for the express lane), and the DB doing double duty as ledger
and queue.

### 3. Express-tier isolation

Express (OTP-class) messages get a **dedicated worker pool** claiming only
`express=true` rows, with a **small batch size** (5 vs ). Bulk traffic
physically cannot occupy express workers, so express delivery latency is
bounded by express arrival rate and express capacity only.

Provisioning follows Little's law: **capacity = workers ÷ per-send latency**
(one worker at 50 ms delivers 20 msg/s). Batch size matters independently:
batches deliver sequentially, so the last message in a batch waits
(batch_size − 1) × latency behind its batchmates — batch size sets the
**tail-latency floor**, worker count sets **throughput**. Both effects are
measured below, and both match their theoretical values.

**Measured:** during an ad-campaign spike that drove *standard* delivery p99
to 14.05 s, *express* p99 in the same minute was 0.77 s — an 18× isolation
gap under one database, one process, one operator. (See
[spikes run](#run-3-spikes--brownout).)

## Failure handling

| Failure | Behavior |
|---|---|
| Transient operator error / send timeout | Requeue (`pending`), bounded by `max_attempts` |
| Terminal operator error | `failed` + **refund in the same transaction** + ledger row — a customer is never charged for an undelivered message |
| Worker crash between claim and resolve | `claimed_at` is a **lease**; a janitor goroutine requeues `sending` rows older than `claim_timeout`. Rescue count is a metric — a sustained nonzero rate means workers are dying |
| Rolling deploy / SIGTERM | HTTP drains, then dispatcher drains (WaitGroup, bounded by shutdown timeout). Outcome writes use `context.WithoutCancel`, so a returned provider call is always recorded; shutdown-interrupted sends are requeued, **not** failed/refunded |
| Multiple replicas migrating concurrently | Migrations run under a Postgres advisory lock on a pinned connection; plus the deploy pipeline runs migrations as a Job *before* rollout |
| Delivery semantics | At-least-once (industry standard for SMS aggregation): a crash between operator ACK and `MarkSent` can re-send. Exactly-once requires operator-side idempotency keys; our message UUID is the natural dedup key |

## Observability

Prometheus metrics (`/metrics`), designed around two hard-won rules:

- **HTTP metrics are labeled by route pattern**,
- **Repository metrics are per logical method**, observed exactly once in a
  `defer`, with a three-way status: `success` / `rejected` (business
  outcomes like insufficient balance) / `error` (real failures) — so error
  alerts don't fire because a customer ran out of credit.

Key series: HTTP rate/latency by route+status, repo op rate/latency,
`sms_gw_messages_delivered_total{outcome=sent|retry|failed|requeued_shutdown|rescued}`.
The SLO signal for the express tier is queue age
(`now() - min(created_at) WHERE status='pending' AND express`).

## Configuration

Viper: defaults → `configs/config.yaml` → `SMS_GW_*` env (highest wins).
Env mapping: `SMS_GW_POSTGRES_HOST` → `postgres.host`.

| Key (env) | Default | Meaning |
|---|---|---|
| `postgres.host/port/dbname/sslmode/user/password` | localhost/5432/sms/disable/–/– | connection |
| `postgres.max_open_connections` | 32 | pool size per process |
| `postgres.migrations_path` | `/migrations` (in image) | migration SQL dir |
| `dispatcher.standard_workers` | 256 | bulk pool size |
| `dispatcher.express_workers` | 32 | express pool size |
| `dispatcher.batch_size` | 50 | bulk claim batch |
| `dispatcher.express_batch_size` | 5 | express claim batch (tail-latency floor) |
| `dispatcher.poll_interval` | 200ms | idle backoff |
| `dispatcher.max_attempts` | 5 | retries before terminal fail + refund |
| `dispatcher.send_timeout` | 5s | per-send provider timeout |
| `dispatcher.claim_timeout` | 2m | `sending` lease; must exceed worst honest attempt |
| `dispatcher.janitor_interval` | 30s | lease-expiry sweep period |
| `provider.latency` | 50ms | mock operator RTT |
| `provider.failure_rate` | 0.02 | mock transient-failure fraction |
| `provider.latency_schedule` | – | e.g. `0s:50ms,330s:200ms,420s:50ms` — brownout simulation |

## Load testing & measured results

Suite: `deployment/load/` (k6 + orchestration script). Design principles:

- **Open-loop load** (`constant-arrival-rate`): k6 fires at the target rate
  regardless of response latency, so server stalls appear as honest tail
  latency instead of quietly reduced load (avoids coordinated omission).
- **Skewed tenants** per the brief: 5 "whales" generate 80% of traffic,
  45 tail users the rest; 5% express.
- **The ledger is the oracle**: after every run the script asserts
  `balance == SUM(credit_transactions)` for every user, and that every
  accepted message is accounted for in some terminal or queued state.

### Run 1: steady (challenge rate)

`RATE=1200 DURATION=3m`, 120 standard workers:

- **216,001 accepted at 1,199.8/s sustained, zero failures.**
- Accept latency: **p50 = 697 µs**, p95 = 1.97 ms, p99 = 42 ms — the accept
  path is one 3-statement ACID transaction; sub-ms medians hold because
  Postgres group-commits concurrent transactions into shared WAL fsyncs.
- **Queue fully drained during the run** (pending = 0 at first post-run poll);
  all 216,001 `sent`; ledger reconciled.
- An earlier run of this test with an env-passthrough bug delivered at only
  ~350/s — and the drain arithmetic (350 ÷ 20 msg/s-per-worker ≈ 17 effective
  workers) pinpointed that the worker override never reached the container.
  Capacity math as a debugging tool.

### Run 2: exhaustion

`MODE=exhaustion FUND=2000`: 50 users funded exactly 2,000 credits each;
300 VUs fire 200,000 sends (2× total budget) unthrottled; provider failure
rate forced to 0 so arithmetic is exact:

- **8,823 req/s sustained** (accept + reject blended), median 24 ms, run
  completed in 22.7 s.
- **Exactly 100,000 accepted, 100,000 rejected (402), 0 users over budget,
  0 users with residual balance, ledger reconciled.** The credit invariant —
  full spend, never overspend — proven under maximal concurrency, including
  50 separate balance-crosses-zero races.
- Comparing against a skewed variant of this run (80% of traffic on 5 rows →
  8,570 req/s): spreading load over 10× more rows bought only ~3%, so at
  this hardware's ~8.8k/s ceiling the binding constraint is systemic
  (connection pool / WAL / shared CPU), **not** single-row lock contention —
  the balance-striping design stays a documented contingency, now with
  evidence it isn't yet needed.

### Run 3: spikes + brownout

`MODE=spikes` — an 8-minute production-shaped timeline, all in one run:
800/s standard + 60/s express baseline throughout; an ad-campaign bulk spike
(0→2,500/s→0) at 1:00–2:30; an OTP express spike (0→300/s→0) at 2:00–4:00
(overlapping the campaign's tail); an operator **brownout** at 5:30–7:00
(mock latency 50 ms → 200 ms → 50 ms via `latency_schedule`). 200 standard /
50 express workers. 618,702 messages, all delivered, ledger reconciled.

Delivery latency (accept → sent), per minute, from the database:

| window | standard p50 / p99 | express p50 / p99 | reading |
|---|---|---|---|
| baseline | 0.05 s / ~0.5 s | 0.05 s / ~0.2 s | healthy |
| **ad spike peak** | **7.73 s / 14.05 s** | **0.11 s / 0.77 s** | **isolation: 18× p99 gap — bulk backlog cannot touch express workers** |
| spike overlap | 3.21 s / 12.7 s | 0.11 s / 0.53 s | 21k OTPs at sub-second p99 while bulk digests its backlog |
| **brownout** | 2.62 s / **18.53 s** | 0.22 s / **1.26 s** | both classes rise — physics: capacity = workers/latency dropped 4× |
| recovery | 0.05 s / 0.31 s | 0.06 s / 0.18 s | backlog paid down within a minute, no stuck messages |

The brownout p99s match a specific model: batches deliver sequentially, so
tail latency ≈ batch_size × per-send latency — standard 100 × 0.2 s ≈ 20 s
(measured 18.5), express 10 × 0.2 s ≈ 2 s (measured 1.26). **Worker count
sets throughput; batch size sets the tail-latency floor under degradation.**
Testable prediction: `BATCH_SIZE=20` should cut standard brownout p99 to
~4 s at the cost of 5× more (cheap) claim queries.

Accept-path SLOs held throughout the entire run, including the
192k-messages minute: overall p99 = 280 ms, express-tagged accepts
p99 = 132 ms.

## Scaling roadmap

The compose/k8s deployments in this repo run a **single Postgres
instance** — sufficient for the challenge and for these benchmarks. The
production shape is one *writer* (primary) plus a standby for failover and
read replicas for report traffic; none of that adds write capacity, because
Postgres replication is single-writer. Capacity-wise the write path is ~15 MB/s
of WAL, far below a modern primary's ceiling (~10–20k msg/s for this
pattern). Levers, in the order they'd be pulled:

1. **Now / cheap**: `LISTEN/NOTIFY` wake-ups for the express lane (kills the
   poll-interval latency); split-mode deployments (API-only vs
   dispatcher-only) with the dispatcher HPA driven by oldest-pending age;
   PgBouncer (transaction pooling) once pod-count × pool-size outgrows
   Postgres's healthy connection budget.
2. **Retention**: daily time-partitioning of `messages`; export cold
   partitions to Parquet on object storage (partition-drop retention, no
   vacuum debt); reports route by the keyset cursor's timestamp — hot pages
   from Postgres, history from the archive.
3. **~10× load or multi-consumer needs**: lift the queue into Kafka via the
   outbox + CDC (Debezium) pattern — Postgres remains the money ledger; the
   delivery workload and analytics fan-out move to the log.

## Repository layout

```
main.go                    entry (cobra)
cmd/                       CLI: serve (API + dispatcher), migrate
configs/                   viper config: defaults, yaml, env binding
internal/api/              HTTP handlers, middleware (pattern-labeled metrics),
                           response plumbing (body caps, uniform errors)
internal/database/         repository: ALL money/queue invariants live here;
                           migrations runner (advisory-locked)
internal/dispatch/         worker pools (express/standard), graceful drain,
                           janitor (lease expiry)
internal/provider/         operator abstraction; mock with latency schedule
internal/metrics/          Prometheus registry + observation helpers
internal/store/            models, sentinel errors
migrations/                versioned schema (partial pending index, lease column)
deployment/k8s/            namespace, postgres, config, migrate Job, gateway,
                           PDB, HPA
deployment/run-minikube.sh fresh-DB bring-up + smoke test
deployment/load/           k6 suite (steady / knee / exhaustion / spikes)
                           + orchestrator with ledger-oracle assertions
Dockerfile                 multi-stage; config + migrations baked in;
                           ENTRYPOINT binary, CMD "serve"
docker-compose.yml         local dev + load-test substrate (env passthrough)
```
# sms-gw
