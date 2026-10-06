CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE IF NOT EXISTS clients (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    api_key_hash  TEXT UNIQUE NOT NULL,   -- SHA-256 hex, not bcrypt
    tier          TEXT NOT NULL DEFAULT 'free',
    rate_limit    INT  NOT NULL DEFAULT 100,
    window_seconds INT NOT NULL DEFAULT 60,
    created_at    TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS request_logs (
    id          BIGSERIAL PRIMARY KEY,
    client_id   UUID REFERENCES clients(id),
    path        TEXT,
    method      TEXT,
    status_code INT,
    allowed     BOOLEAN,
    duration_ms BIGINT,
    created_at  TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_logs_client ON request_logs(client_id, created_at DESC);
