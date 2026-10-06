import http from "k6/http";
import { check } from "k6";

// Used by scripts/benchmark-logging.sh. Load shape comes from --vus/--duration.
// The bench user has a huge rate limit so we measure proxied traffic, not 429s.
const BASE = __ENV.BASE_URL || "http://localhost:8088";

export function setup() {
  const res = http.post(
    `${BASE}/v1/auth/login`,
    JSON.stringify({ email: "bench@example.com", password: "password123" }),
    { headers: { "Content-Type": "application/json" } }
  );
  const token = res.json("token");
  if (!token) throw new Error(`login failed: ${res.status} ${res.body}`);
  return { token };
}

export default function (data) {
  const res = http.get(`${BASE}/v1/proxy/`, { headers: { Authorization: `Bearer ${data.token}` } });
  check(res, { "status 200": (r) => r.status === 200 });
}
