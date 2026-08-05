# Twitch Subscriptions Backend — Architecture & Learning Doc

**Goal:** Build a production-style subscription backend in Go, and actually understand every piece — not just have it exist.

**Audience:** You're very new to Go. So this doc pairs each architectural decision with the Go concept it exercises, so when you sit down to code you already know what you're looking for.

---

## 1. The System, End to End

```
                    React Frontend
                          |
                     HTTP REST API
                          |
                   Go (Gin/Fiber)
                          |
         ------------------------------
         |            |               |
    PostgreSQL      Redis        Worker Service
         |            |               |
         ------------------------------
                    Notifications
```

Four moving pieces, each teaching something different:

| Piece | What it does | What it teaches |
|---|---|---|
| Go API server | Handles HTTP requests, validates input, orchestrates DB/cache | REST design, request lifecycle, error handling |
| PostgreSQL | Source of truth for subscriptions/users/transactions | Schema design, transactions, indexing |
| Redis | Fast read-through cache for hot queries | Cache-aside pattern, invalidation, TTLs |
| Worker | Runs independently of HTTP requests, on a timer | Concurrency, goroutines, scheduled jobs |

The core lesson of this whole project: **the API server, the database, and the worker are three separate concerns that don't trust each other's timing.** The API doesn't expire subscriptions itself (a user might never call the API again after their card expires). The worker doesn't serve HTTP. This separation is *why* production backends look like this.

---

## 2. Go Concepts You'll Need (crash primer)

Since you're new to Go, here's the minimum vocabulary before we write code. Don't try to memorize this — just recognize it when we get there.

- **Structs** — Go's version of a class-ish data container. Your `Subscription` will be a struct with fields like `ID`, `UserID`, `Status`.
- **Interfaces** — a contract of methods, not data. You'll use this for the repository pattern (e.g. `SubscriptionRepository` interface, with a Postgres implementation). This is what lets you swap Postgres for a mock in tests.
- **Error handling** — Go has no exceptions. Every function that can fail returns `(result, error)`, and you check `if err != nil` explicitly, every time. This feels tedious at first; it's also why Go backends rarely have surprise crashes.
- **Goroutines** — lightweight threads, started with `go someFunc()`. Your worker's ticking loop and your queue consumer will each run as a goroutine.
- **Channels** — how goroutines talk to each other safely. You'll use one for a simple in-process notification queue before graduating to SQS.
- **Context (`context.Context`)** — carries cancellation/timeouts through a call chain. Every DB query and HTTP handler will take a `ctx` as its first argument. This is very Go-specific and very important — we'll cover it in depth in Phase 1.
- **defer** — schedules a function to run when the enclosing function returns (e.g. `defer db.Close()`, `defer rows.Close()`). Used constantly for cleanup.

You don't need more than this to start. Everything else (Gin routing, GORM syntax) you'll pick up by doing.

---

## 3. Database Design

### `users`
```
id          SERIAL PRIMARY KEY
username    TEXT NOT NULL
email       TEXT UNIQUE NOT NULL
created_at  TIMESTAMP DEFAULT now()
```

### `creators`
```
id          SERIAL PRIMARY KEY
name        TEXT NOT NULL
created_at  TIMESTAMP DEFAULT now()
```

### `subscriptions`
```
id          SERIAL PRIMARY KEY
user_id     INT REFERENCES users(id)
creator_id  INT REFERENCES creators(id)
plan        TEXT NOT NULL           -- 'monthly', etc.
status      TEXT NOT NULL           -- 'active' | 'cancelled' | 'expired'
start_date  TIMESTAMP NOT NULL
expires_at  TIMESTAMP NOT NULL
auto_renew  BOOLEAN DEFAULT true
created_at  TIMESTAMP DEFAULT now()

UNIQUE (user_id, creator_id) WHERE status = 'active'   -- prevents double-subscribing
```

That partial unique index is the real answer to "check if already subscribed" — instead of doing a SELECT-then-INSERT (which has a race condition if two requests land at once), the database itself rejects the duplicate. This is a good interview talking point: **race conditions in "check then act" logic, and how a constraint eliminates the race instead of just checking harder.**

### `transactions`
```
id                SERIAL PRIMARY KEY
subscription_id   INT REFERENCES subscriptions(id)
amount            NUMERIC(10,2)
currency          TEXT DEFAULT 'usd'
status            TEXT              -- 'pending' | 'succeeded' | 'failed'
created_at        TIMESTAMP DEFAULT now()
```

### Why a separate `transactions` table instead of just a `paid` flag on subscriptions?

Because a subscription and a payment are different lifecycles. A renewal creates a *new* transaction against the *same* subscription. If Stripe payment fails, you want the subscription to stay active (grace period) while a transaction row records the failure. Keeping them separate is what makes "handle a failed renewal without deleting the user's access immediately" possible.

### Transactions (the SQL kind) in the subscribe flow

When a user subscribes, you're doing at least two writes: insert into `subscriptions`, insert into `transactions`. Both must succeed or both must fail — otherwise you get a subscription with no payment record, or a charge with no subscription. This is wrapped in a Postgres transaction (`BEGIN` / `COMMIT` / `ROLLBACK`). In Go with `database/sql`, that's `db.BeginTx(ctx, nil)`, and you `defer tx.Rollback()` immediately after (a committed transaction makes the rollback a no-op, so this is a safe default pattern, not a bug).

---

## 4. API Design

```
POST   /subscriptions                    subscribe
GET    /subscriptions/:id                get one
GET    /users/:id/subscriptions          list a user's subs
DELETE /subscriptions/:id                cancel (soft delete: status = 'cancelled')
POST   /subscriptions/:id/renew          extend expires_at
```

**Subscribe flow, step by step:**
1. Validate the request body (user_id, creator_id, plan all present and sane)
2. Attempt insert — rely on the partial unique index to reject duplicates rather than a separate SELECT check (see above)
3. Calculate `expires_at` = now + 30 days
4. Insert `transactions` row in the same DB transaction
5. Commit
6. Push a "subscription created" event onto the notification queue (see §6) — this happens *after* commit, so you never notify about a subscription that didn't actually save
7. Return 201 with the subscription

**Cancel:** never a hard delete. `UPDATE subscriptions SET status = 'cancelled' WHERE id = $1`. Preserves history for the `transactions` table's foreign key and for analytics/support. This is standard practice — flag it as such in interviews.

**Renew:** `UPDATE subscriptions SET expires_at = expires_at + interval '30 days' WHERE id = $1`. Note it adds 30 days to the *existing* `expires_at`, not to `now()` — otherwise renewing a day early would cost you a day.

---

## 5. Redis Caching Layer

**Pattern: cache-aside (lazy loading).**

```
GET /users/15/subscriptions

  Go checks Redis for key "user:15:subscriptions"
     |
     found -> return immediately
     |
     miss  -> query Postgres
              -> write result into Redis with a TTL (e.g. 60s)
              -> return
```

**The part people get wrong: invalidation.** If you only ever set a TTL and never actively invalidate, a user who cancels a subscription can see stale data for up to 60 seconds. So: every write endpoint (subscribe, cancel, renew) also does `redis.Del("user:{id}:subscriptions")` after a successful commit. This is the classic "cache invalidation is one of the two hard problems in computer science" lesson, made concrete.

Key design: `user:{id}:subscriptions` — scoped per user, not a single global cache, so one user's write doesn't blow away everyone's cache.

---

## 6. Background Worker

This is the piece that makes it a real backend and not just a CRUD API.

```go
ticker := time.NewTicker(1 * time.Minute)
for range ticker.C {
    expireOverdueSubscriptions(ctx, db)
}
```

```sql
UPDATE subscriptions
SET status = 'expired'
WHERE status = 'active' AND expires_at < NOW()
RETURNING id, user_id, creator_id;
```

Two things worth understanding deeply here, both are common interview probes:

**Why polling instead of scheduling an exact-time job per subscription?** Scheduling millions of individual timers doesn't scale and doesn't survive a restart. A periodic sweep is stateless, idempotent (running it twice does nothing extra), and trivially recoverable — if the worker was down for an hour, the next tick catches everything at once.

**Why does this run as a separate process/goroutine from the API, not inside a request handler?** Because expiration must happen even if zero users are hitting the API. This is the "who expires it if nobody logs in" question from your notes, answered directly.

**Scaling concern (good to mention, don't over-build yet):** if you eventually run multiple worker instances, they'd race to process the same rows. The real fix is `SELECT ... FOR UPDATE SKIP LOCKED` so each worker instance grabs a distinct batch. Good to know exists; not needed for a single worker instance in v1.

---

## 7. Notification Queue

```
Subscription Created
        |
   (local) Go channel  →  later:  SQS Queue
        |
     Worker/consumer
        |
   Email Service (stub first, real later)
```

**Build order matters here.** Don't start with real AWS SQS — start with a Go channel or a Redis list acting as a queue *in-process*. The pattern (producer pushes an event, consumer pulls and acts, decoupled from the request) is identical; you're just deferring the AWS account setup and IAM permissions until you already understand the shape of the problem. Swap the channel for SQS once the logic works.

**Why not send the email synchronously inside the POST /subscriptions handler?** Because email delivery is slow and can fail, and you don't want a flaky third-party email API to make your subscribe endpoint slow or unreliable. Decoupling means the user gets their 201 response instantly regardless of email service health.

---

## 8. Folder Structure

```
subscription-service/
  cmd/
    api/            main.go for the HTTP server
    worker/          main.go for the background worker
  internal/
    handlers/        HTTP handlers (thin — parse request, call service, write response)
    services/         business logic (validation, orchestration)
    repository/        DB access (the only layer that writes SQL)
    models/           structs: Subscription, User, Creator, Transaction
    middleware/        auth, logging, rate limiting
  pkg/                shared utilities usable outside this project
  configs/            env/config loading
  Dockerfile
  docker-compose.yml   spins up Postgres + Redis + the app locally
```

**Why layers (handlers → services → repository) instead of one big file?** Each layer has one job, and — critically for testing — you can mock the repository layer in a service test without touching a real database. This is the practical payoff of the "interfaces" concept from §2: `services` depends on a `SubscriptionRepository` interface, not on `*sql.DB` directly.

---

## 9. Build Phases (recommended order)

| Phase | Deliverable | Core Go/backend concept |
|---|---|---|
| 1 | Postgres schema + subscribe/cancel/renew/list endpoints, wrapped in DB transactions | structs, error handling, `database/sql` or GORM, SQL transactions |
| 2 | Redis cache-aside on the list endpoint, invalidation on writes | cache patterns, TTLs |
| 3 | Worker goroutine expiring subscriptions on a ticker | goroutines, `context`, scheduled jobs |
| 4 | In-process queue (channel) → notification stub, then swap to SQS | channels, decoupled processing, AWS SQS basics |
| 5 | JWT auth middleware | middleware chains, request context |
| 6 (bonus) | Stripe sandbox payments, webhooks, rate limiting, metrics endpoint, admin dashboard, unit + integration tests, GitHub Actions CI/CD | integration testing, webhook verification, observability |

Each phase produces something that actually runs, so you're never sitting on a half-wired system.

---

## 10. What This Buys You in an Interview

For each of these, you should be able to give the one-sentence "why," not just describe what the code does:

- **Race condition prevention** → partial unique index instead of check-then-insert
- **Data consistency** → DB transaction around subscribe (subscription + transaction row)
- **Cache invalidation** → active `DEL` on write, not just TTL expiry
- **Why a worker, not a request-triggered job** → expiration must happen without user activity
- **Why an async queue for notifications** → decouple slow/unreliable third-party calls from the request path
- **Why soft delete (`status = cancelled`)** → preserve history, foreign key integrity, support/analytics needs
- **Why layered folders** → testability via interfaces, single-responsibility per layer

---

Next step: Phase 1 — Postgres schema + core CRUD endpoints. When you're ready, we'll set up `docker-compose.yml` for local Postgres/Redis, then write the models and repository layer together, explaining each Go pattern as it shows up.
