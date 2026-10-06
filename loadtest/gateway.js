import http from "k6/http";
import { check } from "k6";

// Sustained mixed traffic: free tier (hits its limit) and premium (stays under).
// Run WITHOUT --vus/--duration: the scenarios below define the load.
export const options = {
  scenarios: {
    free_clients: { executor: "constant-vus", vus: 10, duration: "30s", exec: "freeTraffic" },
    premium_clients: { executor: "constant-vus", vus: 10, duration: "30s", exec: "premiumTraffic" },
  },
};

const BASE = __ENV.BASE_URL || "http://localhost:8088";
const JSON_HDR = { headers: { "Content-Type": "application/json" } };

function ensureUserAndLogin(email, tier) {
  // Signup is idempotent for our purposes: a 409 just means the user exists.
  http.post(`${BASE}/v1/auth/signup`, JSON.stringify({ email, password: "password123", tier }), JSON_HDR);
  const res = http.post(`${BASE}/v1/auth/login`, JSON.stringify({ email, password: "password123" }), JSON_HDR);
  const token = res.json("token");
  if (!token) throw new Error(`login failed for ${email}: ${res.status} ${res.body}`);
  return token;
}

export function setup() {
  return {
    freeToken: ensureUserAndLogin("demo-free@example.com", "free"),
    premiumToken: ensureUserAndLogin("demo-premium@example.com", "premium"),
  };
}

function hit(token) {
  const res = http.get(`${BASE}/v1/proxy/`, { headers: { Authorization: `Bearer ${token}` } });
  check(res, { "allowed or rate limited": (r) => r.status === 200 || r.status === 429 });
}

export function freeTraffic(data) { hit(data.freeToken); }
export function premiumTraffic(data) { hit(data.premiumToken); }
