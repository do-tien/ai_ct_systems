-- +goose Up
-- PIN users, opaque sessions and per-user ownership of clinical records.
--
-- PINs themselves are never stored here: pin_lookup is the deterministic
-- keyed lookup value and pin_hash is the salted verifier string. auth_sessions
-- stores only the SHA-256 digest of the opaque session token. Existing reads
-- are attributed to one disabled "Legacy reader" account, while historical
-- events keep their actor text snapshot with a null user id.
--
-- reads.reader remains the authoritative wire identity until the ownership
-- cutover: reader_user_id is nullable here and is populated only for rows that
-- predate this migration.

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

CREATE INDEX reads_reader_user_id_idx ON reads (reader_user_id);

-- +goose Down
-- reads.reader was never removed, so rollback only removes the new ownership
-- column and the user/session tables. 00005 already drops reader_user_id on its
-- own down, so this must tolerate its absence.
ALTER TABLE events DROP COLUMN actor_user_id;
ALTER TABLE reads DROP COLUMN IF EXISTS reader_user_id;

DROP TABLE auth_sessions;
DROP TABLE users;
