-- +goose Up
-- PIN users, opaque sessions and per-user ownership of clinical records.
--
-- PINs themselves are never stored here: pin_lookup is the deterministic
-- keyed lookup value and pin_hash is the salted verifier string. auth_sessions
-- stores only the SHA-256 digest of the opaque session token. Existing reads
-- are attributed to one disabled "Legacy reader" account, while historical
-- events keep their actor text snapshot with a null user id.

CREATE TABLE users (
    id           BIGSERIAL PRIMARY KEY,
    display_name TEXT NOT NULL
        CHECK (display_name = btrim(display_name) AND length(display_name) BETWEEN 1 AND 100),
    pin_lookup   BYTEA NOT NULL UNIQUE CHECK (octet_length(pin_lookup) = 32),
    pin_hash     TEXT NOT NULL CHECK (length(pin_hash) > 0),
    disabled_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE auth_sessions (
    token_digest BYTEA PRIMARY KEY CHECK (octet_length(token_digest) = 32),
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at)
);

CREATE INDEX auth_sessions_user_id_idx ON auth_sessions (user_id);
CREATE INDEX auth_sessions_expires_at_idx ON auth_sessions (expires_at);

ALTER TABLE reads ADD COLUMN reader_user_id BIGINT REFERENCES users (id);
ALTER TABLE events ADD COLUMN actor_user_id BIGINT REFERENCES users (id) ON DELETE SET NULL;

-- One disabled account receives every pre-migration read. It is created only
-- when legacy rows exist, and its synthetic lookup/hash can never verify a PIN.
WITH legacy AS (
    INSERT INTO users (display_name, pin_lookup, pin_hash, disabled_at)
    SELECT 'Legacy reader', decode(repeat('00', 32), 'hex'), 'disabled', now()
    WHERE EXISTS (SELECT 1 FROM reads)
    RETURNING id
)
UPDATE reads
SET reader_user_id = (SELECT id FROM legacy)
WHERE reader_user_id IS NULL
  AND EXISTS (SELECT 1 FROM legacy);

ALTER TABLE reads ALTER COLUMN reader_user_id SET NOT NULL;

CREATE INDEX reads_reader_user_id_idx ON reads (reader_user_id);

-- The identity guard now protects the owning user instead of the removed text
-- column; every other immutability rule is unchanged.
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
-- Restore the readable historical identity before dropping the user columns so
-- a rollback keeps clinical records interpretable.
ALTER TABLE reads ADD COLUMN reader TEXT;

UPDATE reads
SET reader = users.display_name
FROM users
WHERE users.id = reads.reader_user_id;

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

ALTER TABLE events DROP COLUMN actor_user_id;
ALTER TABLE reads DROP COLUMN reader_user_id;

DROP TABLE auth_sessions;
DROP TABLE users;
