-- migrations/002_add_pending_status.sql
--
-- Phase 7 (Stripe): a subscription is now created in 'pending' state and
-- flips to 'active' only after Stripe confirms the PaymentIntent via
-- webhook. Previously Create returned an already-'active' row.
--
-- Why append a new migration instead of editing 001_init.sql:
-- Migrations are append-only. 001_init has already been applied against
-- real data (dev volume, integration test containers, and eventually
-- production). Editing it in place means the schema you see in source
-- disagrees with the schema that actually got created on any DB that
-- already ran 001 — a classic "works on my machine" bug.
--
-- What this migration changes:
--   1. Adds a new subscription row we also need to add a payment_intent_id
--      column so the webhook can find the row when Stripe pings us. Nullable
--      because pre-Phase-7 rows won't have one.
--   2. Rebuilds the partial unique index so it covers BOTH 'active' AND
--      'pending' rows. If we only guarded 'active', a user could spam
--      Subscribe and pile up N pending rows for the same creator while
--      none of them had paid yet — the whole point of the index is one
--      real-or-in-progress subscription per (user, creator) pair.
--   3. Adds a transactions.stripe_event_id column with a UNIQUE constraint.
--      This is our idempotency key for webhooks: Stripe retries deliveries
--      aggressively (network hiccup on our side, non-2xx from us, etc.),
--      and a UNIQUE constraint at the DB level means a duplicate delivery
--      fails the INSERT atomically — no application-level "have I seen
--      this event id?" cache to get wrong.

BEGIN;

-- Subscriptions: track which Stripe PaymentIntent created this row so the
-- webhook can look the row up by payment_intent.id.
ALTER TABLE subscriptions
    ADD COLUMN payment_intent_id TEXT;

-- One PaymentIntent maps to at most one subscription. Partial index (only
-- on non-NULL) is fine here; NULLs are pre-Phase-7 rows plus any that
-- somehow never got a PI attached.
CREATE UNIQUE INDEX subscriptions_payment_intent_id_key
    ON subscriptions (payment_intent_id)
    WHERE payment_intent_id IS NOT NULL;

-- Drop and recreate the partial unique index to include 'pending' rows.
-- Same shape as before, just a wider WHERE.
DROP INDEX subscriptions_one_active_per_pair;
CREATE UNIQUE INDEX subscriptions_one_active_per_pair
    ON subscriptions (user_id, creator_id)
    WHERE status IN ('active', 'pending');

-- Transactions: record the Stripe event id that produced this transaction
-- update. UNIQUE = duplicate webhook deliveries can't produce duplicate
-- rows — the second insert fails with a unique-violation the handler
-- catches and turns into a 200 OK (already processed).
ALTER TABLE transactions
    ADD COLUMN stripe_event_id TEXT;

CREATE UNIQUE INDEX transactions_stripe_event_id_key
    ON transactions (stripe_event_id)
    WHERE stripe_event_id IS NOT NULL;

COMMIT;
