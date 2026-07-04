#!/usr/bin/env bash
# Load-test the SMS gateway locally (macOS, Docker Compose substrate).
#
# Usage:
#   ./deployment/load/run-load-test.sh                # 1,200 msg/s x 3m (challenge rate)
#   RATE=3000 DURATION=5m ./deployment/load/run-load-test.sh
#   MODE=knee ./deployment/load/run-load-test.sh      # ramp 200 -> 6000/s, find the knee
#   MODE=spikes ./deployment/load/run-load-test.sh    # 8m production-shaped run:
#       baseline + ad-campaign spike + overlapping OTP spike + operator brownout
#
# Why Compose and not Minikube: with the docker driver on a Mac, traffic
# reaches the cluster through kubectl port-forward / an SSH tunnel; at
# ~1000 req/s you would be benchmarking the tunnel. Compose publishes the
# port natively. Minikube stays the functional environment; this is the
# load environment.
#
# Prereqs: Docker Desktop, and k6 (brew install k6).

set -euo pipefail

RATE="${RATE:-1200}"
DURATION="${DURATION:-3m}"
MODE="${MODE:-steady}"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null || die "docker not found"
command -v k6 >/dev/null     || die "k6 not found (brew install k6)"

# --- 1. bring the stack up, sized for the load --------------------------------
# Delivery capacity = workers / provider latency. At 50ms per send, one worker
# delivers 20 msg/s; sustaining 1,200/s ingest therefore needs >= 60 in-flight
# standard workers. The defaults (16) are sized for dev, not load — override:
info "Starting stack (fresh DB), dispatcher sized for ${RATE}/s"
docker compose down -v >/dev/null 2>&1 || true
LATENCY_SCHEDULE=""
DEFAULT_WORKERS=124
DEFAULT_EXPRESS_WORKERS=32
if [[ "$MODE" == "spikes" ]]; then
  # Operator brownout window inside the run: 50ms -> 200ms at 5:30 -> back at 7:00.
  LATENCY_SCHEDULE="0s:50ms,330s:200ms,420s:50ms"
  # Capacity math (workers = rate x latency):
  #  standard: peak 800 baseline + 2500 spike = 3300/s x 50ms = 165 -> 200 workers.
  #    During the brownout only baseline runs: 800/s needs 160 at 200ms -> capacity
  #    200/0.2s = 1000/s: tight on purpose, so the backlog dynamics are visible.
  #  express: peak 360/s x 50ms = 18; at 200ms baseline 60/s needs 12 -> 40 gives
  #    margin in both regimes.
  DEFAULT_WORKERS=124
  DEFAULT_EXPRESS_WORKERS=32
  echo "DEFAULT_WORKERS: $DEFAULT_WORKERS DEFAULT_EXPRESS_WORKERS:$DEFAULT_EXPRESS_WORKERS" 
fi

FAILURE_RATE="0.02"
if [[ "$MODE" == "exhaustion" ]]; then
  # Zero provider failures => zero refunds => exact arithmetic:
  # accepted messages must equal total funded credits, final balances 0.
  FAILURE_RATE="0"
fi
SMS_GW_DISPATCHER_STANDARD_WORKERS="${WORKERS:-$DEFAULT_WORKERS}" \
SMS_GW_DISPATCHER_EXPRESS_WORKERS="${EXPRESS_WORKERS:-$DEFAULT_EXPRESS_WORKERS}" \
SMS_GW_POSTGRES_MAX_OPEN_CONNECTIONS=64 \
SMS_GW_PROVIDER_FAILURE_RATE="$FAILURE_RATE" \
SMS_GW_PROVIDER_LATENCY_SCHEDULE="$LATENCY_SCHEDULE" \
docker compose up --build -d

info "Waiting for the gateway to be ready"
for i in $(seq 1 60); do
  if curl -sf localhost:8080/readyz >/dev/null 2>&1; then break; fi
  [[ $i == 60 ]] && die "gateway never became ready (docker compose logs gateway)"
  sleep 1
done

# --- 2. run k6 -----------------------------------------------------------------
info "Running k6: MODE=$MODE"
FUND="${FUND:-2000}"
k6 run \
  -e BASE_URL=http://localhost:8080 \
  -e RATE="$RATE" -e DURATION="$DURATION" -e MODE="$MODE" -e FUND="$FUND" \
  deployment/load/k6-load.js || true   # keep going: drain check below matters even if thresholds failed

# --- 3. verify the queue drains (delivery kept up with ingest) -----------------
info "Checking queue drain (delivery outcomes vs. accepted)"
for i in $(seq 1 60); do
  PENDING=$(docker compose exec -T db psql -U sms -d sms -tAc \
    "SELECT count(*) FROM messages WHERE status IN ('pending','sending')")
  echo "  in-flight/pending: $PENDING"
  [[ "$PENDING" == "0" ]] && break
  sleep 2
done

info "Final message states + money reconciliation"
docker compose exec -T db psql -U sms -d sms -c \
  "SELECT status, count(*) FROM messages GROUP BY status ORDER BY status;"

# The ledger invariant is the test oracle: for every user,
# balance == sum(ledger). Any mismatch means money was lost or created.
MISMATCH=$(docker compose exec -T db psql -U sms -d sms -tAc "
  SELECT count(*) FROM users u
  WHERE u.balance <> COALESCE((SELECT SUM(amount) FROM credit_transactions t
                               WHERE t.user_id = u.id), 0);")
if [[ "$MISMATCH" == "0" ]]; then
  info "Ledger reconciliation: OK (balance == SUM(ledger) for every user)"
else
  die "Ledger reconciliation FAILED for $MISMATCH user(s) — money invariant broken"
fi

if [[ "$MODE" == "spikes" ]]; then
  info "Delivery latency timeline (p50/p99 seconds from accept to sent, per minute)"
  echo "  Read it against the schedule: ad spike 1:00-2:30, OTP spike 2:00-4:00,"
  echo "  brownout 5:30-7:00. Express p99 should stay low through the AD SPIKE"
  echo "  (isolation working); both classes rise during the BROWNOUT (physics)."
  docker compose exec -T db psql -U sms -d sms -c "
    SELECT to_char(date_trunc('minute', created_at), 'HH24:MI') AS minute,
           express,
           count(*) AS msgs,
           round(percentile_cont(0.5) WITHIN GROUP
             (ORDER BY EXTRACT(EPOCH FROM sent_at - created_at))::numeric, 2) AS p50_s,
           round(percentile_cont(0.99) WITHIN GROUP
             (ORDER BY EXTRACT(EPOCH FROM sent_at - created_at))::numeric, 2) AS p99_s
    FROM messages
    WHERE status = 'sent'
    GROUP BY 1, 2
    ORDER BY 1, 2;"
fi

if [[ "$MODE" == "exhaustion" ]]; then
  info "Exhaustion assertions: overspend is impossible, full spend is possible"
  EXPECTED=$((50 * FUND))
  GOT=$(docker compose exec -T db psql -U sms -d sms -tAc "SELECT count(*) FROM messages")
  # The distribution-independent invariant: NO user has more messages than
  # it was ever funded for. This must hold no matter how attempts are spread.
  OVERSPENT=$(docker compose exec -T db psql -U sms -d sms -tAc "
    SELECT count(*) FROM (
      SELECT user_id, count(*) AS c FROM messages GROUP BY user_id
    ) t WHERE t.c > $FUND")
  NONZERO=$(docker compose exec -T db psql -U sms -d sms -tAc "SELECT count(*) FROM users WHERE balance <> 0")
  echo "  funded credits total : $EXPECTED"
  echo "  messages accepted    : $GOT"
  echo "  users over budget    : $OVERSPENT"
  echo "  users with balance<>0: $NONZERO"
  [[ "$OVERSPENT" == "0" ]] || die "$OVERSPENT user(s) sent more than funded — OVERSPEND: the core invariant is broken"
  [[ "$NONZERO" == "0" ]] || die "$NONZERO user(s) left with unspent balance — full spend not achieved (uniform mode should exhaust everyone)"
  [[ "$GOT" == "$EXPECTED" ]] || die "accepted $GOT != funded $EXPECTED despite zero balances — ledger/message mismatch"
  info "Credit invariant held: every funded credit spent exactly once, not one more."
fi

echo
info "Done. The stack is still up for inspection:"
echo "  curl -s localhost:8080/metrics | grep sms_gw     # Prometheus counters"
echo "  docker compose logs gateway --tail 50"
echo "  docker compose down -v                            # tear down"
