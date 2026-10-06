import http from "k6/http";
import { check } from "k6";
import { Counter } from "k6/metrics";

// Burst 300 requests from ONE free-tier client (limit: 100 req / 60s) across the
// 3 gateway replicas behind nginx.
//   - per-instance (in-memory) limiting would allow ~3 x 100 = 300
//   - shared Redis limiting allows exactly 100 and rejects 200
// The threshold below fails the run (non-zero exit) if the limit is not exact.
const LIMIT = 100;
const TOTAL = 300;

const allowed = new Counter("allowed_200");
const rejected = new Counter("rejected_429");
const other = new Counter("other_status");
const inst = {
  "gateway-1": new Counter("hits_gateway_1"),
  "gateway-2": new Counter("hits_gateway_2"),
  "gateway-3": new Counter("hits_gateway_3"),
};

export const options = {
  scenarios: {
    burst_single_client: {
      executor: "shared-iterations",
      vus: 30,
      iterations: TOTAL,
      maxDuration: "30s",
    },
  },
  thresholds: {
    allowed_200: [`count==${LIMIT}`],
    rejected_429: [`count==${TOTAL - LIMIT}`],
  },
};

const BASE = __ENV.BASE_URL || "http://localhost:8088";
const JSON_HDR = { headers: { "Content-Type": "application/json" } };

export function setup() {
  // Fresh account each run so the previous run's 60s window can't skew the count.
  const email = `burst-${Date.now()}@example.com`;
  http.post(`${BASE}/v1/auth/signup`, JSON.stringify({ email, password: "password123" }), JSON_HDR);
  const res = http.post(`${BASE}/v1/auth/login`, JSON.stringify({ email, password: "password123" }), JSON_HDR);
  const token = res.json("token");
  if (!token) throw new Error(`login failed: ${res.status} ${res.body}`);
  return { token };
}

export default function (data) {
  const res = http.get(`${BASE}/v1/proxy/`, {
    headers: { Authorization: `Bearer ${data.token}` },
  });
  if (res.status === 200) allowed.add(1);
  else if (res.status === 429) rejected.add(1);
  else other.add(1);

  const id = res.headers["X-Gateway-Instance"];
  if (id && inst[id]) inst[id].add(1);

  check(res, { "200 or 429": (r) => r.status === 200 || r.status === 429 });
}

export function handleSummary(data) {
  const c = (n) => (data.metrics[n] ? data.metrics[n].values.count : 0);
  const out = [
    "",
    "=== Multi-instance rate-limit proof ===",
    `Allowed (200):  ${c("allowed_200")}   (expected ${LIMIT})`,
    `Rejected (429): ${c("rejected_429")}   (expected ${TOTAL - LIMIT})`,
    `Other status:   ${c("other_status")}`,
    `Hits per replica: gateway-1=${c("hits_gateway_1")} gateway-2=${c("hits_gateway_2")} gateway-3=${c("hits_gateway_3")}`,
    "",
  ].join("\n");
  return { stdout: out };
}
