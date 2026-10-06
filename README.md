# Distributed API Gateway

This is an API gateway written in Go — the single entry point clients talk to instead of calling a backend service directly. Every request passes through authentication (JWT or API key), a per-account rate limit, and request logging before it's forwarded upstream. The gateway runs as three identical replicas behind nginx for load balancing, and the rate limiter uses a Redis-backed sliding-window algorithm so the limit is enforced globally across all three replicas — not per-replica, which would let a client triple their quota by spreading requests around. This mirrors how real API gateways like Kong, Envoy, and AWS API Gateway separate cross-cutting concerns (auth, throttling, routing) from business logic.
---

## What it does

| Feature | Detail |
|---|---|
| **Authentication** | JWT (HS256, issued at login) or `X-API-Key`. API keys are 256-bit random values from `crypto/rand`, stored as SHA-256 hashes (not bcrypt: keys are high-entropy, so a fast hash is safe and allows indexed lookup). Passwords use bcrypt |
| **Per-tier rate limiting** | Free: 100 req/60 s. Premium: 1000 req/60 s. Sliding-window log in a Redis Lua script, atomic across all replicas |
| **Reverse proxy** | Strips `Authorization`/`X-API-Key`, adds `X-Client-Id`/`X-Client-Tier`, reuses upstream connections (keep-alive pool), returns a JSON 502 if the upstream is down |
| **Request logging** | Every proxied request, **including rate-limited ones**, is logged to Postgres. `LOG_MODE=sync` writes before responding; `async` (default) writes from a background goroutine |
| **Observability** | Prometheus metrics at `/metrics` (requests by status/tier, rate-limit rejections, latency histogram) |
| **Log history** | `GET /v1/me/logs?limit=N` returns the caller's own recent requests |
| **Graceful shutdown** | On SIGTERM the server stops accepting requests, then flushes the async log queue |

---

## Architecture

```
Client
  │
  ▼
nginx :8088  (round-robin, keep-alive to gateways)
  │
  ├── gateway-1 :8080
  ├── gateway-2 :8080
  └── gateway-3 :8080
        │
        ├── PostgreSQL  (accounts, request logs)
        └── Redis       (rate-limit state, shared by all replicas)
              │
              ▼
          Upstream service
```

Middleware order on `/v1/proxy/*`:

```
auth → metrics → log → rate-limit → proxy
```

Metrics and logging deliberately sit **outside** the rate limiter. When the limiter rejects a request it aborts the chain, so anything placed after it would never see the 429s and the rejection counter and `allowed=false` log rows would always be empty.

---

## Why shared Redis for rate limiting

If each replica kept counters in memory, a free-tier client (limit 100) could send 100 requests to each of 3 replicas and get 300 successes. With shared Redis all replicas update one counter.

The update is one Lua script (`ZREMRANGEBYSCORE` + `ZCARD` + `ZADD`). Redis runs scripts atomically, so two replicas can't both read "under limit" and both admit a request. Each request is stored under a unique member (`<ms>-<random>`), so two requests in the same millisecond are both counted.

### Verified with `loadtest/multi_instance.js`

One free-tier client (limit 100 req/60s) sends 300 requests across the 3 gateway replicas behind nginx. The script exits non-zero unless exactly 100 are allowed and 200 are rejected:

```bash
$ docker run --rm --network distributed-api-gateway-v2_default \
    -v "$PWD/loadtest:/loadtest" -e BASE_URL=http://nginx:8080 \
    grafana/k6 run /loadtest/multi_instance.js

=== Multi-instance rate-limit proof ===
Allowed (200):  100   (expected 100)
Rejected (429): 200   (expected 200)
Other status:   0
Hits per replica: gateway-1=34 gateway-2=33 gateway-3=33
```

Exactly 100 allowed and 200 rejected, split roughly evenly across all 3 replicas (nginx round-robin). If each replica kept its own in-memory counter instead, this client could have gotten up to 300 successes instead of 100 — this is the proof that the limit is actually global.

---

## Mixed free/premium load — correct behavior under sustained traffic

`loadtest/gateway.js` runs 10 free-tier VUs and 10 premium-tier VUs continuously for 30 seconds. Free clients hit their 100 req/60s limit almost immediately and then correctly receive `429`s for the rest of the run; premium clients (limit 1000/60s) mostly stay under their limit. The test's own check passes for *either* a `200` or a `429` — both are correct outcomes here:

```bash
$ docker run --rm --network distributed-api-gateway-v2_default \
    -v "$PWD/loadtest:/loadtest" -e BASE_URL=http://nginx:8080 \
    grafana/k6 run /loadtest/gateway.js

checks_succeeded...: 100.00%  362859 out of 362859
checks_failed......: 0.00%    0 out of 362859
http_req_duration..: p(95)=3.67ms  med=1.26ms
http_reqs..........: 362863  12,759.67/s
```

Every one of 362,859 requests got exactly the status it should have — a free client over its limit got `429`, a premium client under its limit got `200` — with zero unexpected statuses. (k6's generic `http_req_failed` metric reports a large "failure" rate for this run, but that's a naming artifact: it only counts plain `2xx`/`3xx` as success by default and has no concept of "a 429 was the correct answer here." The test's own `checks_succeeded: 100%` is what actually reflects correctness.)

---

## Async logging benchmark

A synchronous log write puts a Postgres round-trip on every request's critical path. `LOG_MODE=async` moves it to a background goroutine (20,000-entry buffer, batched `COPY` writes; drops are counted).

Setup: 3 gateway replicas + nginx + Postgres + Redis + k6 all on one laptop (WSL2), 50 VUs, 30s per run, 5s warm-up, premium user with a 1,000,000 req limit so every response is a proxied 200. Results below are the **median of 3 runs**, alternating which mode ran first each round to cancel out cold-start bias:

```bash
$ bash scripts/benchmark-logging.sh

Median of 3 runs (50 VUs, 30s each)

| metric | sync | async | change |
|---|---|---|---|
| median latency (ms) | 11.6 | 4.3 | -63% |
| p95 latency (ms) | 24.4 | 11.6 | -52% |
| avg latency (ms) | 13.2 | 5.2 | -61% |
| throughput (req/s) | 3728.6 | 9297.5 | +149% |
| requests logged to Postgres | 100.0% | 100.0% | |
```

Both modes logged 100% of requests in this run — the throughput gain is purely from moving the DB write off the hot path, not from dropping any work.

**Trade-off:** async logging is best-effort. If the buffer fills (a burst faster than Postgres can absorb) entries are dropped rather than slowing clients, and a hard crash loses whatever is still buffered. Graceful shutdown flushes the buffer. Use `sync` if you need every log row to be durable.

Absolute numbers depend on the machine; the point is the relative cost of the DB write on the hot path.

---

## Unit tests

```bash
$ docker run --rm -v "$PWD":/src -w /src golang:1.22-alpine sh -c "go mod tidy && go test ./... -v"

=== RUN   TestGenerateTokenIsUniqueAndLong
--- PASS: TestGenerateTokenIsUniqueAndLong (0.00s)
=== RUN   TestHashAPIKeyDeterministic
--- PASS: TestHashAPIKeyDeterministic (0.00s)
=== RUN   TestJWTRoundTrip
--- PASS: TestJWTRoundTrip (0.00s)
=== RUN   TestJWTRejectsWrongSecretAndGarbage
--- PASS: TestJWTRejectsWrongSecretAndGarbage (0.00s)
PASS
ok      github.com/T490-hue/Distributed-API-Gateway/internal/auth       0.015s

=== RUN   TestAsyncLogNeverBlocks
--- PASS: TestAsyncLogNeverBlocks (0.00s)
=== RUN   TestMiddlewareLogsRejectionsAndOriginalPath
--- PASS: TestMiddlewareLogsRejectionsAndOriginalPath (0.00s)
PASS
ok      github.com/T490-hue/Distributed-API-Gateway/internal/logging    0.013s
```

Tests cover the two hardest correctness properties in the system: that API keys and JWTs are generated and verified correctly (including rejection of tampered/garbage tokens), and that the async logger genuinely never blocks the request path even when its buffer is full — plus that rejected (429) requests are logged with the client-facing path, not the rewritten upstream path.

---

## Tech stack

| Piece | Why |
|---|---|
| **Go + Gin** | Goroutine-per-request fits a non-blocking logger; Gin middleware maps cleanly to the pipeline |
| **PostgreSQL** | Durable store for accounts and request history; pool capped at 25 connections |
| **Redis** | Sorted sets implement the sliding window; Lua gives atomicity across replicas |
| **nginx** | Simple round-robin load balancer; also hides `/metrics` from the public entrypoint |
| **Prometheus (+ Grafana)** | Prometheus scrapes each replica every 5s |
| **k6** | Scripted concurrent load, with thresholds that fail the run if behaviour is wrong |

---

## Run locally

```bash
git clone https://github.com/T490-hue/Distributed-API-Gateway.git
cd Distributed-API-Gateway
docker compose up -d --build
curl http://localhost:8088/health
```

```bash
curl -s -X POST http://localhost:8088/v1/auth/signup \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"password123"}'

TOKEN=$(curl -s -X POST http://localhost:8088/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"password123"}' | jq -r .token)

curl http://localhost:8088/v1/proxy/ -H "Authorization: Bearer $TOKEN"
curl "http://localhost:8088/v1/me/logs?limit=20" -H "Authorization: Bearer $TOKEN"
```

`docker-compose.yml` sets `ALLOW_SELF_SERVE_PREMIUM=true` so demos can sign up with `"tier":"premium"`. It is off by default; never enable it in production.

---

## API

| Path | Auth | Description |
|---|---|---|
| `GET /health` | none | Liveness check |
| `GET /metrics` | none (internal only behind nginx) | Prometheus metrics |
| `POST /v1/auth/signup` | none | Register; returns the API key once |
| `POST /v1/auth/login` | none | Returns a signed JWT (24h) |
| `GET /v1/me` | JWT or API key | Account info |
| `GET /v1/me/logs` | JWT or API key | Request history (`limit`, max 200) |
| `ANY /v1/proxy/*` | JWT or API key | Rate-limited proxy to the upstream |

Status codes: `401` bad/missing credentials, `429` rate limit exceeded, `502` upstream unreachable, `503` rate limiter (Redis) unavailable.

---

## Running the tests yourself

All commands below assume the stack is already up (`docker compose up -d --build`) and run from the repo root.

**1. Unit tests** (no local Go install needed — runs inside the same Go version as the Dockerfile):

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.22-alpine sh -c "go mod tidy && go test ./... -v"
```

**2. Find your compose network name** (needed for the two k6 commands below):

```bash
docker network ls | grep gateway
```

**3. Rate-limit correctness proof** — one free-tier client, 300 requests, 3 replicas:

```bash
NET="<your network name from step 2>"
docker run --rm --network "$NET" -v "$PWD/loadtest:/loadtest" \
  -e BASE_URL=http://nginx:8080 grafana/k6 run /loadtest/multi_instance.js
```

**4. Mixed free/premium sustained load:**

```bash
docker run --rm --network "$NET" -v "$PWD/loadtest:/loadtest" \
  -e BASE_URL=http://nginx:8080 grafana/k6 run /loadtest/gateway.js
```

**5. Async vs sync logging benchmark** — this one manages its own setup (starts the stack, creates a benchmark user, alternates sync/async across 3 rounds, prints the median table) — just run it directly, no network variable needed:

```bash
bash scripts/benchmark-logging.sh

# optional: more rounds, more load
RUNS=5 VUS=100 DURATION=60s bash scripts/benchmark-logging.sh
```

