-- migrations/001_init.sql
-- Initial schema for the subscription service.
-- Auto-run by the Postgres container on first boot (see docker-compose.yml).

CREATE TABLE users (
    id         SERIAL PRIMARY KEY,
    username   TEXT NOT NULL,
    email      TEXT UNIQUE NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT now()
);

CREATE TABLE creators (
    id         SERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT now()
);

CREATE TABLE subscriptions (
    id         SERIAL PRIMARY KEY,
    user_id    INT NOT NULL REFERENCES users(id),
    creator_id INT NOT NULL REFERENCES creators(id),
    plan       TEXT NOT NULL,                          -- 'monthly', etc.
    status     TEXT NOT NULL,                          -- 'active' | 'cancelled' | 'expired'
    start_date TIMESTAMP NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    auto_renew BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMP NOT NULL DEFAULT now()
);

-- THE IMPORTANT PART:
-- A *partial* unique index — unique only over rows where status = 'active'.
-- This means: one active subscription per (user, creator) pair, but any number
-- of cancelled/expired ones can exist (which is exactly what you want for history).
--
-- WHY THIS INSTEAD OF "SELECT then INSERT"?
-- Naive approach: SELECT to check if an active sub exists, and INSERT if not.
-- Two concurrent requests can both run the SELECT (both see "no active sub"),
-- and both then INSERT — you end up with two active subs. That's a classic
-- TOCTOU race (time-of-check vs time-of-use).
--
-- The database itself can't be raced: a UNIQUE INDEX is enforced atomically.
-- One INSERT wins; the other fails with a unique-violation error, which our
-- Go code catches and converts into a 409 Conflict. The race is impossible
-- by construction, not just "unlikely if we're careful".
CREATE UNIQUE INDEX subscriptions_one_active_per_pair
    ON subscriptions (user_id, creator_id)
    WHERE status = 'active';

CREATE TABLE transactions (
    id              SERIAL PRIMARY KEY,
    subscription_id INT NOT NULL REFERENCES subscriptions(id),
    amount          NUMERIC(10, 2) NOT NULL,
    currency        TEXT NOT NULL DEFAULT 'usd',
    status          TEXT NOT NULL,                     -- 'pending' | 'succeeded' | 'failed'
    created_at      TIMESTAMP NOT NULL DEFAULT now()
);

-- A couple of seed rows so you can POST to /subscriptions immediately.
INSERT INTO users (username, email) VALUES
    ('alice', 'alice@example.com'),
    ('bob',   'bob@example.com');

INSERT INTO creators (name) VALUES
    ('Ninja'),
    ('Pokimane');
