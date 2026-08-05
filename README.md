# Subscription Service

A production-style subscription backend in Go, built as a learning project.
Think Twitch/Patreon-style creator subscriptions: users subscribe to creators
on a monthly plan, subscriptions expire, get cancelled, or renew.

Full architecture and rationale live in
[`subscription-service-architecture.md`](./subscription-service-architecture.md).

**Status: Phase 1 complete** — core CRUD API backed by Postgres.
Redis is provisioned but unused. Worker, notification queue, auth, and
payments come in later phases.

---

## Stack

- **Go 1.22** with [Gin](https://github.com/gin-gonic/gin) for HTTP
- **PostgreSQL 16** (via `database/sql` + `lib/pq`)
- **Redis 7** (provisioned via docker-compose, not yet wired up)
- **Docker Compose** for local infrastructure

## Project layout

```
subscription-service/
├── cmd/
│   └── api/                  # HTTP server entry point (main.go)
├── internal/
│   ├── handlers/             # HTTP layer (Gin) — parse, call service, write JSON
│   ├── services/             # Business logic — validation, orchestration
│   ├── repository/           # SQL layer — the only place SQL lives
│   └── models/               # Plain data structs shared across layers
├── migrations/
│   └── 001_init.sql          # Schema, auto-applied on first Postgres boot
├── docker-compose.yml        # Postgres + Redis
├── go.mod / go.sum
└── subscription-service-architecture.md
```

The dependency arrow only points one way:
`handlers → services → repository → models`. Every layer depends on the one
below it, and `services` depends on the repository **interface** (not the
concrete Postgres type), which is what makes the business logic testable
without a real database.

---

## Getting started

### Prerequisites

- Docker Desktop (or any Docker + Compose)
- Go 1.22+

### 1. Start Postgres & Redis

```bash
docker compose up -d
```

The Postgres container maps to **host port 5433** (5432 is often busy on
macOS). Schema in `migrations/001_init.sql` is auto-applied on first boot
and seeds two users (`alice`, `bob`) and two creators (`Ninja`, `Pokimane`).

To reset the database completely (drops the volume so migrations re-run):

```bash
docker compose down -v && docker compose up -d
```

### 2. Install Go dependencies

```bash
go mod tidy
```

### 3. Run the API

```bash
go run ./cmd/api
```

Server listens on `:8080`. Uses `DATABASE_URL` env var if set, otherwise
defaults to `postgres://subs:subs@localhost:5433/subscriptions?sslmode=disable`.

Health check:

```bash
curl localhost:8080/healthz
```

---

## API

All endpoints return JSON. Errors come back as `{"error": "..."}`.

| Method | Path                          | Description                          |
|--------|-------------------------------|--------------------------------------|
| POST   | `/subscriptions`              | Subscribe (user → creator)           |
| GET    | `/subscriptions/:id`          | Fetch a single subscription          |
| GET    | `/users/:id/subscriptions`    | List all subscriptions for a user    |
| DELETE | `/subscriptions/:id`          | Cancel (soft delete)                 |
| POST   | `/subscriptions/:id/renew`    | Extend `expires_at` by 30 days       |
| GET    | `/healthz`                    | Liveness probe                       |

### Example: subscribe

```bash
curl -X POST localhost:8080/subscriptions \
  -H 'content-type: application/json' \
  -d '{"user_id":1,"creator_id":1,"plan":"monthly"}'
```

Response `201 Created`:

```json
{
  "id": 1,
  "user_id": 1,
  "creator_id": 1,
  "plan": "monthly",
  "status": "active",
  "start_date": "2026-08-04T...",
  "expires_at": "2026-09-03T...",
  "auto_renew": true,
  "created_at": "2026-08-04T..."
}
```

Attempting to subscribe the same user to the same creator while an active
subscription exists returns `409 Conflict` — enforced by a **partial unique
index** in Postgres, not an application-level check (see below).

### Status codes

| Code | When                                             |
|------|--------------------------------------------------|
| 200  | Successful GET / renew                           |
| 201  | Successful subscribe                             |
| 204  | Successful cancel                                |
| 400  | Missing/invalid body or URL params               |
| 404  | Subscription not found (or not active for renew) |
| 409  | Active subscription already exists for this pair |
| 500  | Anything else                                    |

---

## Design decisions worth knowing

- **Race-free "already subscribed" check.** A partial unique index on
  `(user_id, creator_id) WHERE status = 'active'` makes double-subscribing
  impossible at the DB level. Two concurrent inserts can't both win — one
  gets a unique-violation the repository translates into `ErrDuplicateActive`.
- **Atomic subscribe.** The subscription row and the initial transaction
  row are inserted inside a single `BEGIN`/`COMMIT`, with `defer tx.Rollback()`
  as the safety net. Half-writes are impossible.
- **Soft delete on cancel.** `status = 'cancelled'`, never `DELETE`. Preserves
  history for the transactions foreign key and future analytics.
- **Renewal adds to `expires_at`, not `now()`.** Renewing a day early
  shouldn't cost you a day.
- **Interface-based repository.** `services` depends on
  `repository.SubscriptionRepository` (interface), not `*postgresRepo`, so
  a test can inject a fake without touching Postgres.

---

## What's next

See `subscription-service-architecture.md` §9 for the phase plan.

- Phase 2 — Redis cache-aside on the list endpoint + write-invalidation
- Phase 3 — Background worker that expires overdue subscriptions on a ticker
- Phase 4 — In-process notification queue (channel), later swapped for SQS
- Phase 5 — JWT auth middleware
- Phase 6 — Stripe sandbox, webhooks, rate limiting, metrics, tests, CI/CD
