# Subscription Service

A production-style subscription backend in Go, built as a learning project.
Think Twitch/Patreon-style creator subscriptions: users subscribe to creators
on a monthly plan, subscriptions expire, get cancelled, or renew.

Full architecture and rationale live in
[`subscription-service-architecture.md`](./subscription-service-architecture.md).

**Status: Phase 4 complete** — core CRUD API backed by Postgres, with a
Redis cache-aside layer on the list endpoint, a background worker that
expires overdue subscriptions on a timer, and an in-process notification
queue that fires "subscribed" and "expired" events to a stub consumer.
Auth and payments come in later phases.

---

## Stack

- **Go 1.22** with [Gin](https://github.com/gin-gonic/gin) for HTTP
- **PostgreSQL 16** (via `database/sql` + `lib/pq`)
- **Redis 7** (via [`go-redis/v9`](https://github.com/redis/go-redis)) — cache-aside on the user-list endpoint
- **Docker Compose** for local infrastructure

## Project layout

```
subscription-service/
├── cmd/
│   ├── api/                  # HTTP server entry point (main.go)
│   └── worker/               # Background expiration sweeper (main.go)
├── internal/
│   ├── handlers/             # HTTP layer (Gin) — parse, call service, write JSON
│   ├── services/             # Business logic — validation, orchestration, cache coord.
│   ├── repository/           # SQL layer — the only place SQL lives
│   ├── cache/                # Redis key format helpers (shared by API + worker)
│   ├── notifications/        # In-process event queue + stub consumer
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

Redis is now a **required** runtime dependency (Phase 2). The API still
runs if Redis is unreachable — it degrades to serving directly from
Postgres and logs a warning — but the cache-aside behavior is gone.

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

Server listens on `:8080`. Env vars:

- `DATABASE_URL` — Postgres DSN (default `postgres://subs:subs@localhost:5433/subscriptions?sslmode=disable`)
- `REDIS_ADDR` — Redis host:port (default `localhost:6379`)

Health check:

```bash
curl localhost:8080/healthz
```

### 4. Run the worker (Phase 3)

In a **second terminal** — the worker is a separate binary and does not
serve HTTP. It shares the same env vars as the API.

```bash
go run ./cmd/worker
```

You'll see one log line per tick (every 30s by default), even on ticks
that expire nothing, so it's obvious the process is alive.

---

## API

All endpoints return JSON. Errors come back as `{"error": "..."}`.

| Method | Path                          | Description                          |
|--------|-------------------------------|--------------------------------------|
| POST   | `/subscriptions`              | Subscribe (user → creator)           |
| GET    | `/subscriptions/:id`          | Fetch a single subscription          |
| GET    | `/users/:id/subscriptions`    | List a user's subscriptions (cached) |
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

## Phase 2: Redis Caching

`GET /users/:id/subscriptions` is served through a **cache-aside** layer
in the service. On a cache hit we return the cached JSON without touching
Postgres; on a miss we fetch from Postgres and populate Redis with a TTL.

### Key format

Every user's list lives under a single key:

```
user:{id}:subscriptions
```

Reads, writes, and invalidations all go through one helper —
`userListKey(userID)` in `internal/services/subscription_service.go` — so
the key format is defined in exactly one place. A typo can't silently split
"the read path" from "the invalidation path."

### TTL: 60 seconds (as a backstop)

Cached entries expire after 60 seconds. The TTL is **not** the primary
freshness mechanism — active `DEL` on writes is. The TTL exists so that if
an invalidation is ever missed (bug, transient Redis error, code path we
forgot), staleness is bounded to 60 seconds instead of forever.

### Which paths invalidate

After a **successful Postgres commit**, these paths `DEL user:{id}:subscriptions`
for the affected user:

- `POST   /subscriptions`               (Subscribe)
- `DELETE /subscriptions/:id`           (Cancel — repo returns `user_id` via `RETURNING`)
- `POST   /subscriptions/:id/renew`     (Renew)

Invalidation runs **after** the DB commit, never before. Invalidating first
would create a window where a concurrent read could repopulate the cache
with pre-write data.

### Why the cache lives in the service, not the repository

The repository stays Postgres-only — that keeps it single-purpose and easy
to mock. Caching is a business/product decision ("how stale is acceptable
here?"), not a storage concern; other future callers of the same repository
methods may want a different (or no) cache.

### Known failure mode: DB commit succeeds but Redis DEL fails

If Postgres commits and then the Redis `DEL` fails (Redis down, network
blip, process killed between the two calls), the client still receives a
success response — the write genuinely succeeded, and returning an error
would make the client retry an already-committed operation. The failed
`DEL` is logged, and worst-case staleness is bounded by the 60s TTL.

This is a **deliberate, documented tradeoff**, not an oversight. A proper
fix (durable retry queue for failed invalidations, or an outbox pattern)
is deferred until Phase 4 introduces the notification queue — same shape
of problem, same infra.

### How to verify the cache

Run the API (`go run ./cmd/api`) and use another shell:

```bash
# Fresh slate
docker exec subs_redis redis-cli FLUSHALL

# 1. First read — cache MISS, populates Redis
curl -s localhost:8080/users/1/subscriptions >/dev/null
docker exec subs_redis redis-cli GET user:1:subscriptions   # → JSON
docker exec subs_redis redis-cli TTL user:1:subscriptions   # → ~60 (counts down)
# API log line: "cache MISS user:1:subscriptions"

# 2. Second read — cache HIT (no Postgres query)
curl -s localhost:8080/users/1/subscriptions >/dev/null
# API log line: "cache HIT user:1:subscriptions"

# 3. Watch Redis traffic in real time (optional, second terminal)
docker exec -it subs_redis redis-cli MONITOR
# Then re-run the curl. You should see a GET and NO SET — the absence of
# SET is the proof that Postgres wasn't touched.

# 4. Invalidation on write
curl -s -X POST localhost:8080/subscriptions \
     -H 'content-type: application/json' \
     -d '{"user_id":1,"creator_id":2,"plan":"monthly"}'
docker exec subs_redis redis-cli GET user:1:subscriptions   # → (nil)

# Next read repopulates with the fresh list
curl -s localhost:8080/users/1/subscriptions >/dev/null
docker exec subs_redis redis-cli GET user:1:subscriptions   # → JSON with new sub

# 5. Cancel also invalidates
SUB_ID=<paste an active id>
curl -s -X DELETE localhost:8080/subscriptions/$SUB_ID
docker exec subs_redis redis-cli GET user:1:subscriptions   # → (nil)
```

The four signals that together prove the cache is real:

1. `GET user:1:subscriptions` is `(nil)` before the first request and JSON after.
2. `TTL` shows a positive countdown from ~60.
3. `MONITOR` shows a `GET` with no `SET` on repeat reads.
4. `GET` returns `(nil)` immediately after any write for that user.

---

## Phase 3: Background Worker (subscription expiration)

A second binary at `cmd/worker/` sweeps overdue subscriptions on a timer
and flips them from `active` to `expired`. It shares the repository +
service + cache-key code with the API, so its cache invalidations are
byte-identical to the ones the API performs.

### The sweep query

```sql
UPDATE subscriptions
SET status = 'expired'
WHERE status = 'active' AND expires_at < NOW()
RETURNING id, user_id;
```

One atomic UPDATE. `RETURNING` gives the worker back the `(id, user_id)`
of every row it actually changed, which is all it needs to invalidate
the affected users' Redis lists.

### Why this is idempotent (safe to re-run)

The `WHERE status = 'active'` predicate makes double-processing
impossible: once a row is flipped to `expired`, the next sweep's WHERE no
longer matches it, so `RETURNING` returns nothing for that id. Same
"push the invariant into the DB" instinct as the Phase 1 partial unique
index — instead of tracking "have I processed this already?" in
application memory, we let the predicate itself rule it out.

### Cadence

- **Local / testing:** 30 seconds. Short enough to see a tick within a
  reasonable time when manually testing.
- **Production:** 1–5 minutes. Expiration doesn't need to be
  second-precise; a subscription being "expired" 90 seconds late is
  invisible to users, and a longer interval means fewer no-op sweeps.

Every tick logs, including zero-count ones, so it's obvious the worker
is alive:

```
sweep OK in 3.2ms: 0 expired
sweep OK in 4.1ms: 2 expired, ids=[7 12]
```

### Concurrency: what if two workers run at once?

Postgres takes a row-level lock during UPDATE. If two workers fire
simultaneously, one waits for the other and then sees `status` is no
longer `active` for those rows, matching zero. **DB-level exactly-once
processing is preserved without any extra locking on our side.**

`SELECT ... FOR UPDATE SKIP LOCKED` would be needed only if we wanted N
workers to **partition** the sweep (each grabbing a distinct batch to
parallelize a large backlog). We don't — one worker on a timer is fine
at this scale. Documented as a Phase 3 gap; the minimal fix if we ever
needed it would be `SELECT ... FOR UPDATE SKIP LOCKED LIMIT N` followed
by `UPDATE` by id.

### End-to-end test (do this yourself)

Terminal A: `go run ./cmd/api`
Terminal B: `go run ./cmd/worker`
Terminal C: shell for verification.

```bash
# 1. Create a subscription
curl -s -X POST localhost:8080/subscriptions \
     -H 'content-type: application/json' \
     -d '{"user_id":1,"creator_id":1,"plan":"monthly"}'

# 2. Backdate expires_at into the past so the next sweep catches it.
#    (In production this would happen naturally over 30 days.)
docker exec subs_postgres psql -U subs -d subscriptions -c \
  "UPDATE subscriptions SET expires_at = NOW() - interval '1 minute'
   WHERE user_id = 1 AND status = 'active';"

# 3. Warm the cache so we can prove the worker invalidates it
curl -s localhost:8080/users/1/subscriptions >/dev/null
docker exec subs_redis redis-cli GET user:1:subscriptions   # → JSON, still 'active'

# 4. Wait up to 30s for the next worker tick. In Terminal B you should see:
#    sweep OK in ...: 1 expired, ids=[<the id>]

# 5. Verify the DB row flipped and the cache was invalidated
docker exec subs_postgres psql -U subs -d subscriptions -c \
  "SELECT id, status, expires_at FROM subscriptions WHERE user_id = 1;"
# → status = 'expired'

docker exec subs_redis redis-cli GET user:1:subscriptions
# → (nil)   ← the worker's DEL landed

# 6. And the API returns the updated status
curl -s localhost:8080/users/1/subscriptions | jq
# → status: "expired" on that row
```

If step 6 still shows `active`, either the worker isn't running (check
Terminal B for tick logs) or the DB row wasn't actually backdated
(re-run step 2 and check with psql).

---

## Phase 4: Notification Queue

Producers (the service layer) publish `Event`s after a successful DB
commit. A consumer goroutine reads them off a buffered Go channel and
calls a stub `NotifyCreator` that logs a line per event. This is the
same producer/consumer shape SQS will have — the swap later touches
only `main.go`.

### Event shape and paths that publish

```go
type Event struct {
    SubscriptionID int
    UserID         int
    CreatorID      int
    Type           EventType   // "subscribed" | "expired"
}
```

- `POST /subscriptions` → publishes `subscribed` after commit (API process).
- Worker sweep → publishes one `expired` per row it flipped (worker process).

Both use the exact same `Publisher` interface; the service layer doesn't
know or care whether the queue behind it is a Go channel or SQS.

### Non-blocking Publish + dropped-event behavior

Publish uses `select { case ch <- ev: default: log }` — a **non-blocking
send**. If the buffer (default 128) is full, the event is dropped and a
`DROPPED event` line is logged.

Why non-blocking: publish is called inside an HTTP request handler.
Blocking here would couple the API's response latency to the consumer's
health — exactly the coupling the queue exists to prevent.

What "dropped" costs: **only** the notification for that specific event.
The DB commit already happened, so the subscription is real; the creator
just doesn't get a notification for that one signup. This is a documented
tradeoff of the same class as Phase 2's Redis DEL failure and Phase 3's
worker-restart-loses-timing gap. Under normal load the buffer is never
full — a `DROPPED` line is your signal to raise the buffer or move to SQS.

### Durability gap (documented)

Events live in an in-memory channel. **If the process crashes, any events
still in the buffer are lost.** Same is true if a clean shutdown happens
mid-drain: the consumer stops on `ctx.Done` and any remaining events log
as "dropped in-flight."

What changes when we swap to SQS:
- `Publish` becomes an SQS `SendMessage`; the buffer disappears and
  durability becomes the queue's problem, not our process's.
- The consumer becomes an SQS long-poll loop with visibility timeouts
  and explicit `DeleteMessage` on success. Failed handlers re-appear
  after the visibility timeout and eventually go to a dead-letter queue.
- Producers can live in different processes/hosts without needing a
  shared in-memory channel. The current per-process queue collapses to
  one shared queue.
- The `Publisher` interface stays; only its constructor call in
  `main.go` changes.

### Consumer idempotency (deliberately not built yet)

`NotifyCreator` currently just logs — being called twice for the same
event is harmless (you see the log line twice). Once it becomes a real
email send, duplicates would mean duplicate emails: annoying, not
corrupting. The clean fix is a UUID stamped at Publish time plus
consumer-side "have I seen this ID?" tracking. Deferred — not worth
building against a log stub, and SQS FIFO queues have message dedupe
built in that may handle it for free after the swap.

### How to verify

Terminal A: `go run ./cmd/api`
Terminal B: shell.

```bash
curl -s -X POST localhost:8080/subscriptions \
     -H 'content-type: application/json' \
     -d '{"user_id":1,"creator_id":1,"plan":"monthly"}'
```

In Terminal A you should see, in this order and effectively instantly
(< 1ms between the two under normal load):

```
[GIN]  ... | 201 |  ... POST     /subscriptions
notifications: would notify creator=1 about subscribed of subscription=<id> (user=1)
```

Why "effectively instantly": the consumer goroutine is already parked on
`<-ch` waiting; the moment Publish sends onto the channel, the runtime
schedules the consumer and it prints its line. Under heavy load or if
`NotifyCreator` grew slow, you could see the second line lag behind by
the amount of consumer backlog — but the HTTP `201` still returns
immediately because Publish never waits.

To watch the expired path, follow the Phase 3 test recipe (backdate an
`expires_at`, wait for the worker tick). In the **worker's** terminal
you'll see, right after `sweep OK ... expired`:

```
notifications: would notify creator=1 about expired of subscription=<id> (user=1)
```

To force the dropped-event path (mostly curiosity): lower the buffer to
1 in `cmd/api/main.go` and hammer the endpoint with a loop; you'll see
`DROPPED event` lines when the consumer can't keep up.

---

## What's next

See `subscription-service-architecture.md` §9 for the phase plan.

- ~~Phase 2 — Redis cache-aside on the list endpoint + write-invalidation~~ ✅
- ~~Phase 3 — Background worker that expires overdue subscriptions on a ticker~~ ✅
- ~~Phase 4 — In-process notification queue (channel), later swapped for SQS~~ ✅
- Phase 5 — JWT auth middleware
- Phase 6 — Stripe sandbox, webhooks, rate limiting, metrics, tests, CI/CD
