-- migrations/003_add_role_and_status_changed_at.sql
--
-- Phase 11 (admin dashboard). Two additive changes bundled in one
-- migration because they land together and there's no reason to split
-- their transactional lifetimes.
--
-- 1. users.role — the role claim baked into JWTs at login time.
--    Default 'user' so every existing row is a regular user; the admin
--    is created manually via SQL after this migration runs (see the
--    README Phase 11 verification recipe).
--
--    WHY DEFAULT 'user' AND NOT NULLABLE:
--    A nullable role would give us THREE states (user / admin / NULL)
--    where we want two. NULL would then propagate into the JWT as
--    empty string, which RequireAdmin would (correctly) reject —
--    turning every un-migrated pre-Phase-11 user into a locked-out
--    account. Default 'user' + NOT NULL means the migration is
--    idempotent-friendly: run it against any row set and everyone
--    keeps their current access level.
--
-- 2. subscriptions.status_changed_at — timestamp of the last status
--    transition. Enables the admin /stats endpoint's "cancelled in
--    last 24h" and "expired in last 24h" without a separate audit
--    table. UPDATEs to status (Cancel, MarkPaymentSucceeded,
--    MarkPaymentFailed, ExpireOverdue) must all SET this column too;
--    if a future path forgets, the stats undercount but nothing
--    breaks.
--
--    Default NOW() means existing rows all get "now" as their last
--    status change, which is inaccurate for historical rows but
--    acceptable — the "in last 24h" queries are forward-looking
--    from the migration date. If we ever cared about accurate
--    history for pre-migration rows we'd populate via a backfill
--    query (e.g. copy expires_at for expired, created_at for
--    everything else) as a separate step.

BEGIN;

ALTER TABLE users
    ADD COLUMN role TEXT NOT NULL DEFAULT 'user';

-- Only two roles for now. A CHECK constraint keeps typos out ('adnim',
-- 'Admin', 'ADMIN') — a case-sensitivity or spelling mistake at
-- insert time would silently lock the user out otherwise.
ALTER TABLE users
    ADD CONSTRAINT users_role_check CHECK (role IN ('user', 'admin'));

ALTER TABLE subscriptions
    ADD COLUMN status_changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

COMMIT;
