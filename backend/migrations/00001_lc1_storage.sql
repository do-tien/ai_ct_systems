-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION lc1_finite_triple(value double precision[]) RETURNS boolean
LANGUAGE sql
IMMUTABLE
CALLED ON NULL INPUT
AS $$
  SELECT COALESCE(
    array_ndims(value) = 1
    AND array_length(value, 1) = 3
    AND array_lower(value, 1) = 1
    AND value[1] IS NOT NULL
    AND value[2] IS NOT NULL
    AND value[3] IS NOT NULL
    AND value[1] > '-Infinity'::float8 AND value[1] < 'Infinity'::float8
    AND value[2] > '-Infinity'::float8 AND value[2] < 'Infinity'::float8
    AND value[3] > '-Infinity'::float8 AND value[3] < 'Infinity'::float8,
    false
  )
$$;
-- +goose StatementEnd

CREATE TABLE cases (
    id                     BIGSERIAL PRIMARY KEY,
    source                 TEXT NOT NULL,
    study_instance_uid     TEXT NOT NULL,
    series_instance_uid    TEXT NOT NULL UNIQUE,
    orthanc_series_id      TEXT NOT NULL,
    frame_of_reference_uid TEXT NOT NULL,
    status                 TEXT NOT NULL DEFAULT 'awaiting_read'
        CHECK (status IN ('awaiting_read', 'initial_locked', 'finalized')),
    finalized_at           TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE reads (
    id         BIGSERIAL PRIMARY KEY,
    case_id    BIGINT NOT NULL REFERENCES cases (id),
    kind       TEXT NOT NULL CHECK (kind IN ('initial', 'final')),
    reader     TEXT NOT NULL,
    locked_at  TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (case_id, kind)
);

CREATE TABLE marks (
    id                BIGSERIAL PRIMARY KEY,
    read_id           BIGINT NOT NULL REFERENCES reads (id),
    p1_mm             DOUBLE PRECISION[3] NOT NULL CHECK (lc1_finite_triple(p1_mm)),
    p2_mm             DOUBLE PRECISION[3] NOT NULL CHECK (lc1_finite_triple(p2_mm)),
    center_mm         DOUBLE PRECISION[3] NOT NULL CHECK (lc1_finite_triple(center_mm)),
    diameter_mm       DOUBLE PRECISION NOT NULL
        CHECK (diameter_mm > 0 AND diameter_mm < 'Infinity'::float8),
    origin            TEXT NOT NULL
        CHECK (origin IN ('reader', 'reader_after_ai', 'from_ai_candidate')),
    ai_candidate_id   TEXT,
    view_plane_normal DOUBLE PRECISION[3],
    note              TEXT,
    CHECK (p1_mm <> p2_mm),
    CHECK (view_plane_normal IS NULL OR lc1_finite_triple(view_plane_normal)),
    CHECK (
        origin = 'from_ai_candidate' AND ai_candidate_id IS NOT NULL AND btrim(ai_candidate_id, E' \t\r\n') <> ''
        OR origin <> 'from_ai_candidate' AND ai_candidate_id IS NULL
    )
);

CREATE TABLE ai_jobs (
    id          BIGSERIAL PRIMARY KEY,
    case_id     BIGINT NOT NULL REFERENCES cases (id),
    status      TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    result      JSONB,
    error_code  TEXT,
    stderr_tail TEXT,
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);

CREATE TABLE candidate_decisions (
    read_id        BIGINT NOT NULL REFERENCES reads (id),
    candidate_id   TEXT NOT NULL CHECK (btrim(candidate_id, E' \t\r\n') <> ''),
    decision       TEXT NOT NULL CHECK (decision IN ('accepted', 'dismissed')),
    dismiss_reason TEXT,
    decided_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (read_id, candidate_id),
    CHECK (
        decision = 'dismissed' AND dismiss_reason IS NOT NULL
            AND length(btrim(dismiss_reason, E' \t\r\n')) > 0
        OR decision = 'accepted' AND dismiss_reason IS NULL
    )
);

CREATE TABLE events (
    id      BIGSERIAL PRIMARY KEY,
    case_id BIGINT REFERENCES cases (id),
    actor   TEXT NOT NULL,
    action  TEXT NOT NULL,
    detail  JSONB,
    at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX marks_read_id_idx ON marks (read_id);
CREATE INDEX ai_jobs_case_id_id_idx ON ai_jobs (case_id, id);
CREATE INDEX events_case_id_id_idx ON events (case_id, id);

-- +goose Down
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS candidate_decisions;
DROP TABLE IF EXISTS ai_jobs;
DROP TABLE IF EXISTS marks;
DROP TABLE IF EXISTS reads;
DROP TABLE IF EXISTS cases;
DROP FUNCTION IF EXISTS lc1_finite_triple(double precision[]);
