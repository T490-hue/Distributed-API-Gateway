#!/usr/bin/env bash
# Sync vs async request-logging benchmark.
# Same load, same endpoint; only LOG_MODE changes. Runs RUNS rounds, alternating
# which mode goes first (so cold-start effects don't favour one mode), and
# prints the per-mode median.
#
#   bash scripts/benchmark-logging.sh            # 3 rounds, 30s, 50 VUs
#   RUNS=5 VUS=100 DURATION=60s bash scripts/benchmark-logging.sh
set -euo pipefail
cd "$(dirname "$0")/.."

DURATION=${DURATION:-30s}
VUS=${VUS:-50}
RUNS=${RUNS:-3}
PROJECT=${COMPOSE_PROJECT_NAME:-$(basename "$PWD" | tr '[:upper:]' '[:lower:]')}
NETWORK=${NETWORK:-${PROJECT}_default}
RESULTS=loadtest/results
mkdir -p "$RESULTS"

k6() {  # k6 <extra args...> <script>
  docker run --rm --user "$(id -u):$(id -g)" --network "$NETWORK" \
    -v "$PWD/loadtest:/loadtest" -e BASE_URL=http://nginx:8080 \
    grafana/k6 run "$@"
}

echo "Starting stack..."
docker compose up -d postgres redis backend nginx >/dev/null

echo "Preparing benchmark user (premium, rate limit 1,000,000)..."
until curl -sf http://localhost:8088/health >/dev/null; do sleep 1; done
curl -s -o /dev/null -X POST http://localhost:8088/v1/auth/signup \
  -H 'Content-Type: application/json' \
  -d '{"email":"bench@example.com","password":"password123","tier":"premium"}' || true
docker compose exec -T postgres psql -U gateway -d gateway -q \
  -c "UPDATE clients SET rate_limit=1000000 WHERE email='bench@example.com';"

run_once() {
  local mode=$1 round=$2
  echo "--- round $round  LOG_MODE=$mode"
  LOG_MODE=$mode docker compose up -d --force-recreate gateway-1 gateway-2 gateway-3 >/dev/null 2>&1
  docker compose restart nginx >/dev/null 2>&1   # nginx caches upstream IPs at startup
  until curl -sf http://localhost:8088/health >/dev/null; do sleep 1; done

  # warm-up (not recorded): fills connection pools, JIT-like caches
  k6 --duration 5s --vus "$VUS" /loadtest/bench.js >/dev/null 2>&1 || true

  sleep 3   # let the async writer finish the warm-up batch
  docker compose exec -T postgres psql -U gateway -d gateway -q -c "TRUNCATE request_logs;" >/dev/null

  k6 --duration "$DURATION" --vus "$VUS" \
     --summary-export "/loadtest/results/${mode}-${round}.json" \
     /loadtest/bench.js >"/tmp/k6-${mode}-${round}.log" 2>&1 \
    || { echo "k6 FAILED:"; tail -25 "/tmp/k6-${mode}-${round}.log"; exit 1; }

  sleep 5   # let the async writer drain, then count what actually reached Postgres
  docker compose exec -T postgres psql -U gateway -d gateway -tA \
    -c "SELECT count(*) FROM request_logs;" > "$RESULTS/${mode}-${round}.rows"
}

rm -f "$RESULTS"/sync-* "$RESULTS"/async-*
for i in $(seq 1 "$RUNS"); do
  if (( i % 2 )); then run_once sync "$i"; run_once async "$i"
  else                 run_once async "$i"; run_once sync "$i"; fi
done

python3 - "$RESULTS" "$RUNS" "$VUS" "$DURATION" <<'PY'
import json, statistics, sys
d, runs, vus, dur = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
def load(mode, i):
    m = json.load(open(f"{d}/{mode}-{i}.json"))["metrics"]
    g = lambda k: m[k].get("values", m[k])
    t = g("http_req_duration"); r = g("http_reqs"); c = g("checks")
    bad = c.get("fails", 0)
    ok = c.get("passes", 0)
    proxied = r["count"] - 1   # minus the login call in k6 setup(), which isn't logged
    logged = int(open(f"{d}/{mode}-{i}.rows").read().strip() or 0)
    return dict(p95=t["p(95)"], med=t["med"], avg=t["avg"], rps=r["rate"],
                err=bad / max(ok + bad, 1), logged=100.0 * logged / max(proxied, 1))
rows = {}
for mode in ("sync", "async"):
    rs = [load(mode, i) for i in range(1, runs + 1)]
    if any(x["err"] > 0.01 for x in rs):
        print(f"WARNING: {mode} had >1% non-200 responses - results are not valid")
    rows[mode] = {k: statistics.median(x[k] for x in rs) for k in ("med", "avg", "p95", "rps", "logged")}
print(f"\nMedian of {runs} runs ({vus} VUs, {dur} each)\n")
print("| metric | sync | async | change |\n|---|---|---|---|")
for k, label, better_low in (("med","median latency (ms)",1),("p95","p95 latency (ms)",1),("avg","avg latency (ms)",1),("rps","throughput (req/s)",0)):
    s, a = rows["sync"][k], rows["async"][k]
    print(f"| {label} | {s:.1f} | {a:.1f} | {(a-s)/s*100:+.0f}% |")
print(f"| requests logged to Postgres | {rows['sync']['logged']:.1f}% | {rows['async']['logged']:.1f}% | |")
if rows["async"]["logged"] < 99:
    print("\nNOTE: async dropped log entries; part of its speed-up is skipped work. Report the logged % alongside the numbers.")
PY
echo; echo "Raw per-run results: $RESULTS/   Full k6 logs: /tmp/k6-*.log"
