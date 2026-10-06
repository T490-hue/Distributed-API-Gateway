# Deploying a free live demo

Goal: a public URL for your README and resume at **$0**. Free tiers change, so
check each provider's pricing page before relying on them.

**Recommended stack (all have free tiers, none needs a Fly-style paid plan):**

| Piece | Service | Notes |
|---|---|---|
| Gateway (Docker) | [Render](https://render.com) free web service | Spins down after ~15 min idle; first request after that takes ~30-60 s to wake |
| Postgres | [Neon](https://neon.tech) free | Doesn't expire (Render's free Postgres does, after ~30 days) |
| Redis | [Upstash](https://upstash.com) free | TLS URL (`rediss://...`), supported via `REDIS_URL` |
| Upstream | `https://httpbin.org` | Public echo service; occasionally slow |

> The free web service runs **one** instance. The 3-replica rate-limit proof
> (`loadtest/multi_instance.js`) runs locally with Docker Compose; say so in the
> README rather than implying the live demo has 3 replicas.

## Steps

1. **Neon:** create a project, copy the connection string (it already includes
   `sslmode=require`). Load the schema once:
   ```bash
   psql "<neon-connection-string>" -f scripts/schema.sql
   ```
2. **Upstash:** create a Redis database, copy the `rediss://default:<password>@<host>:6379` URL.
3. **Render:** New → Web Service → connect your GitHub repo → Runtime **Docker**
   → Instance type **Free** → Health check path `/health`. Add environment variables:

   | Variable | Value |
   |---|---|
   | `DATABASE_URL` | Neon connection string |
   | `REDIS_URL` | Upstash `rediss://...` URL |
   | `JWT_SECRET` | output of `openssl rand -base64 32` |
   | `BACKEND_URL` | `https://httpbin.org` |
   | `LOG_MODE` | `async` |
   | `GIN_MODE` | `release` |

   Do **not** set `ALLOW_SELF_SERVE_PREMIUM` (signups are free-tier only).
   Render injects `PORT`; the gateway reads it automatically.
4. **Test:**
   ```bash
   curl https://<your-app>.onrender.com/health
   curl -X POST https://<your-app>.onrender.com/v1/auth/signup \
     -H 'Content-Type: application/json' \
     -d '{"email":"demo@example.com","password":"password123"}'
   ```
5. Put the URL in the README under "Live demo", with a note that the first
   request may take up to a minute (free instance waking up).

## Other options

- **ngrok** (`ngrok http 8088`): exposes your local Compose stack; laptop must stay on, URL changes on restart. Good for a live interview demo.
- **Fly.io / Railway:** both have moved to usage-based billing with a payment method required, so they aren't a reliable $0 option.

## Production hardening checklist

- [ ] `JWT_SECRET`: generated (`openssl rand -base64 32`), never committed
- [ ] Postgres and Redis passwords: generated, not the `gateway`/blank dev defaults
- [ ] `ALLOW_SELF_SERVE_PREMIUM` unset (otherwise anyone can sign up as premium)
- [ ] `GIN_MODE=release`
- [ ] `/metrics` not publicly reachable (the bundled nginx returns 404 for it; on Render, put it behind auth or a private service)
- [ ] `SetMaxOpenConns` sized to your Postgres plan (Neon free has a low connection limit; lower it if you see "too many connections")
- [ ] HTTPS: Render terminates TLS for you
- [ ] Container runs as non-root (`USER app` in the Dockerfile)
