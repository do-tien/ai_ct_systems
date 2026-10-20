-- +goose Up
-- Bind every read to the authenticated user who owns it.
--
-- reads.reader was the POC's fixed wire identity. From this migration on the
-- owner is a real users row: reader_user_id becomes mandatory, the read guard
-- protects it, and the free-text column is removed. Existing rows without an
-- owner are attributed to one disabled "Legacy reader" account, reusing the
-- account migration 00004 may already have created instead of inserting a
-- second row with the same synthetic pin_lookup.

WITH null_rows AS (
    SELECT 1 FROM reads WHERE reader_user_id IS NULL LIMIT 1
), existing AS (
    -- Prefer the disabled account 00004 created, but accept any disabled user
    -- that already holds the synthetic lookup so pin_lookup stays unique.
    SELECT id
    FROM users
    WHERE disabled_at IS NOT NULL
      AND (display_name = 'Legacy reader'
           OR pin_lookup = decode(repeat('00', 32), 'hex'))
    ORDER BY (display_name = 'Legacy reader') DESC, id
    LIMIT 1
), created AS (
    INSERT INTO users (display_name, pin_lookup, pin_hash, disabled_at)
    SELECT 'Legacy reader', decode(repeat('00', 32), 'hex'), 'disabled', now()
    WHERE EXISTS (SELECT 1 FROM null_rows)
      AND NOT EXISTS (SELECT 1 FROM existing)
    RETURNING id
), legacy AS (
    SELECT id FROM existing
    UNION ALL
    SELECT id FROM created
)
UPDATE reads
SET reader_user_id = (SELECT id FROM legacy LIMIT 1)
WHERE reader_user_id IS NULL;

ALTER TABLE reads ALTER COLUMN reader_user_id SET NOT NULL;

-- Read identity now includes the owner id and no longer references the
-- removed text column. The trigger itself is unchanged.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION lc1_guard_read_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.locked_at IS NOT NULL THEN
            RAISE EXCEPTION 'locked read is immutable'
                USING ERRCODE = '23514', CONSTRAINT = 'lc1_read_immutable';
        END IF;
        RETURN OLD;
    END IF;

    -- A read's identity never changes, locked or not.
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.case_id IS DISTINCT FROM OLD.case_id
        OR NEW.kind IS DISTINCT FROM OLD.kind
        OR NEW.reader_user_id IS DISTINCT FROM OLD.reader_user_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'read identity is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'lc1_read_immutable';
    END IF;

    -- A locked read keeps its timestamp; reads have no other mutable column, so
    -- this leaves only a true no-op update.
    IF OLD.locked_at IS NOT NULL AND NEW.locked_at IS DISTINCT FROM OLD.locked_at THEN
        RAISE EXCEPTION 'locked read is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'lc1_read_immutable';
    END IF;

    RETURN NEW;
END
$$;
-- +goose StatementEnd

ALTER TABLE reads DROP COLUMN reader;

-- +goose Down
-- Restore the 00002/00004 text identity from the current owner's display name
-- so rolling back keeps every read attributed.
ALTER TABLE reads ADD COLUMN reader TEXT;

UPDATE reads r
SET reader = u.display_name
FROM users u
WHERE u.id = r.reader_user_id;

ALTER TABLE reads ALTER COLUMN reader SET NOT NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION lc1_guard_read_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.locked_at IS NOT NULL THEN
            RAISE EXCEPTION 'locked read is immutable'
                USING ERRCODE = '23514', CONSTRAINT = 'lc1_read_immutable';
        END IF;
        RETURN OLD;
    END IF;

    -- A read's identity never changes, locked or not.
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.case_id IS DISTINCT FROM OLD.case_id
        OR NEW.kind IS DISTINCT FROM OLD.kind
        OR NEW.reader IS DISTINCT FROM OLD.reader
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'read identity is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'lc1_read_immutable';
    END IF;

    -- A locked read keeps its timestamp; reads have no other mutable column, so
    -- this leaves only a true no-op update.
    IF OLD.locked_at IS NOT NULL AND NEW.locked_at IS DISTINCT FROM OLD.locked_at THEN
        RAISE EXCEPTION 'locked read is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'lc1_read_immutable';
    END IF;

    RETURN NEW;
END
$$;
-- +goose StatementEnd

ALTER TABLE reads DROP COLUMN reader_user_id;
