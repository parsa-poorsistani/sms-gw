// k6 load test for the SMS gateway.
//
//   k6 run deployment/load/k6-load.js                       # 1,200 msg/s for 3m (challenge rate)
//   k6 run -e RATE=3000 -e DURATION=5m deployment/load/k6-load.js
//   k6 run -e MODE=knee deployment/load/k6-load.js          # ramp 200 -> 6000/s to find the knee
//   k6 run -e MODE=exhaustion -e FUND=2000 deployment/load/k6-load.js
//   MODE=spikes (via run-load-test.sh): an 8-minute production-shaped run —
//       constant baseline (standard + express), an ad-campaign bulk spike,
//       an OTP express spike overlapping its tail, and an operator brownout
//       window (50ms -> 200ms -> 50ms) — all in one timeline:
//
//         0:00      baseline: 800/s standard + 60/s express, whole run
//         1:00-2:30 AD CAMPAIGN: bulk ramps 0 -> 2500/s -> 0     (spike alone,
//         2:00-4:00 OTP SPIKE: express ramps 0 -> 300/s -> 0      then overlap,
//                                                                 then express alone)
//         5:30-7:00 OPERATOR BROWNOUT: provider latency 200ms (set by runner
//                   via SMS_GW_PROVIDER_LATENCY_SCHEDULE), baseline continues
//         7:00-8:00 recovery: watch the backlog drain
//       # fund each user with EXACTLY $FUND credits, fire ~2x that many
//       # sends, expect 402s once balances hit zero. The runner script then
//       # asserts messages == users x FUND and every balance == 0: the
//       # credit invariant, tested under real concurrency.
//
// Design notes (see docs/ARCHITECTURE.md):
//  - OPEN-LOOP load: constant-arrival-rate fires requests at the target rate
//    regardless of response latency. Closed-loop tools (fixed VU count) stop
//    sending when the server stalls, hiding exactly the bad moments
//    ("coordinated omission"). Open-loop p99 is honest.
//  - SKEWED tenants, per the challenge: a few whales generate most traffic,
//    a long tail sends rarely.
//  - 5% express traffic, so the express-isolation claim is exercised.

import http from 'k6/http';
import { check, fail } from 'k6';
import { Counter, Trend } from 'k6/metrics';

// In exhaustion mode a 402 is a CORRECT answer, not a failure; without this,
// k6 counts every expected rejection in http_req_failed (observed: 74.87%).
if ((__ENV.MODE || 'steady') === 'exhaustion') {
  http.setResponseCallback(http.expectedStatuses(200, 201, 202, 402));
}

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const RATE = parseInt(__ENV.RATE || '1200');
const DURATION = __ENV.DURATION || '3m';
const MODE = __ENV.MODE || 'steady';
const FUND = parseInt(__ENV.FUND || '2000');

const NUM_WHALES = 5;    // heavy senders: ~80% of traffic
const NUM_TAIL = 45;     // light senders: ~20% of traffic
const EXPRESS_FRACTION = 0.05;

const accepted = new Counter('sms_accepted');
const rejected402 = new Counter('sms_rejected_balance');
const acceptLatency = new Trend('sms_accept_latency', true);

const spikeScenarios = {
  baseline_standard: {
    executor: 'constant-arrival-rate',
    exec: 'standardMsg',
    rate: 800, timeUnit: '1s', duration: '8m',
    preAllocatedVUs: 200, maxVUs: 2000,
  },
  baseline_express: {
    executor: 'constant-arrival-rate',
    exec: 'expressMsg',
    rate: 60, timeUnit: '1s', duration: '8m',
    preAllocatedVUs: 30, maxVUs: 500,
  },
  ad_campaign: {
    executor: 'ramping-arrival-rate',
    exec: 'standardMsg',
    startTime: '1m', startRate: 0, timeUnit: '1s',
    preAllocatedVUs: 300, maxVUs: 2000,
    stages: [
      { target: 2500, duration: '20s' }, // ramp up
      { target: 2500, duration: '50s' }, // sustain
      { target: 0, duration: '20s' },    // ramp down (ends ~2:30)
    ],
  },
  otp_spike: {
    executor: 'ramping-arrival-rate',
    exec: 'expressMsg',
    startTime: '2m', startRate: 0, timeUnit: '1s',
    preAllocatedVUs: 100, maxVUs: 1000,
    stages: [
      { target: 300, duration: '15s' },  // overlaps ad-campaign tail (2:00-2:30)
      { target: 300, duration: '90s' },  // express spike alone (2:30-3:45)
      { target: 0, duration: '15s' },    // ends ~4:00
    ],
  },
};

export const options = {
  scenarios:
    MODE === 'spikes'
      ? spikeScenarios
      : MODE === 'exhaustion'
      ? {
          exhaustion: {
            // Fire exactly 2x the total funded budget, as fast as 300 VUs can:
            // maximal contention on the whales' balance rows near zero.
            executor: 'shared-iterations',
            vus: 300,
            iterations: (NUM_WHALES + NUM_TAIL) * FUND * 2,
            maxDuration: '15m',
          },
        }
      : MODE === 'knee'
      ? {
          knee: {
            executor: 'ramping-arrival-rate',
            startRate: 200,
            timeUnit: '1s',
            preAllocatedVUs: 500,
            maxVUs: 4000,
            stages: [
              { target: 1200, duration: '1m' },
              { target: 3000, duration: '2m' },
              { target: 6000, duration: '2m' },
            ],
          },
        }
      : {
          steady: {
            executor: 'constant-arrival-rate',
            rate: RATE,
            timeUnit: '1s',
            duration: DURATION,
            preAllocatedVUs: Math.min(RATE, 1000),
            maxVUs: 4000,
          },
        },
  thresholds:
    MODE === 'spikes'
      ? {
          http_req_failed: ['rate<0.01'],
          // Accept path must stay fast for everyone even during spikes...
          http_req_duration: ['p(99)<500'],
          // ...and express accepts specifically must not degrade:
          'http_req_duration{express:true}': ['p(99)<250'],
        }
      : MODE === 'exhaustion'
      ? { http_req_failed: ['rate<0.01'] } // 402s are EXPECTED here, latency SLO not the point
      : {
          // Accept-path SLO: enqueue (one ACID tx) should be fast even at rate.
          http_req_duration: ['p(99)<250', 'p(50)<50'],
          http_req_failed: ['rate<0.01'],
        },
};

// setup() runs once: create the tenant population and fund it generously so
// the run measures throughput, not balance exhaustion. (To test exhaustion
// behavior under load, fund with less than RATE * seconds and watch 402s.)
export function setup() {
  const users = [];
  const total = NUM_WHALES + NUM_TAIL;
  for (let i = 0; i < total; i++) {
    const res = http.post(`${BASE}/users`, JSON.stringify({ name: `load-${i}` }), {
      headers: { 'Content-Type': 'application/json' },
    });
    check(res, { 'user created': (r) => r.status === 201 }) ||
      fail(`user creation failed: ${res.status} ${res.body}`);
    const id = res.json('id');
    // steady/knee: fund generously so the run measures throughput.
    // exhaustion: fund EXACTLY, so running dry is the scenario.
    const amount = MODE === 'exhaustion' ? FUND : 10_000_000;
    http.post(`${BASE}/users/${id}/credit`, JSON.stringify({ amount: amount }), {
      headers: { 'Content-Type': 'application/json' },
    });
    users.push(id);
  }
  return { users };
}

function pickUser(users) {
  if (MODE === 'exhaustion') {
    // Uniform: every user receives ~2x its budget in attempts (mean 4000,
    // sd ~63 over 200k trials), so "every balance reaches zero" is testable.
    return users[Math.floor(Math.random() * users.length)];
  }
  // steady/knee: 80% of requests from the whales, 20% spread over the tail.
  if (Math.random() < 0.8) {
    return users[Math.floor(Math.random() * NUM_WHALES)];
  }
  return users[NUM_WHALES + Math.floor(Math.random() * NUM_TAIL)];
}

export function standardMsg(data) {
  sendOne(data, false);
}

export function expressMsg(data) {
  sendOne(data, true);
}

export default function (data) {
  sendOne(data, Math.random() < EXPRESS_FRACTION);
}

function sendOne(data, express) {
  const userId = pickUser(data.users);
  const res = http.post(
    `${BASE}/users/${userId}/messages`,
    JSON.stringify({
      phone: `+98912${String(Math.floor(Math.random() * 10_000_000)).padStart(7, '0')}`,
      body: express ? 'OTP 123456' : 'load test message',
      express: express,
    }),
    { headers: { 'Content-Type': 'application/json' }, tags: { express: String(express) } },
  );

  const ok = check(res, {
    'accepted or cleanly rejected':
      (r) => r.status === 202 || (MODE === 'exhaustion' && r.status === 402),
  });
  if (res.status === 202) {
    accepted.add(1);
    acceptLatency.add(res.timings.duration);
  } else if (res.status === 402) {
    rejected402.add(1); // only expected if you deliberately underfund setup()
  }
}

export function teardown(data) {
  const res = http.get(`${BASE}/users/${data.users[0]}/messages?limit=50`);
  check(res, { 'report readable': (r) => r.status === 200 });
}
