-- +goose Up
-- Candidate-decision and finalized-case guards (LC1-13).
--
-- The guards extend the LC1-11 immutability boundary to the two remaining
-- physician-owned records: the candidate decisions of a read and a finalized
-- case. They are a data-integrity boundary, not a privilege system: the owner
-- can still drop them. They make a locked read's decisions and a finalized case
-- immutable through application code, direct runtime SQL and reassignment.
-- +goose StatementBegin
CREATE FUNCTION lc1_guard_decision_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    locked boolean;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT r.locked_at IS NOT NULL INTO locked
        FROM public.reads r WHERE r.id = NEW.read_id FOR UPDATE;
        IF locked IS TRUE THEN
            RAISE EXCEPTION 'read is locked'
                USING ERRCODE = '23514', CONSTRAINT = 'lc1_decision_read_locked';
        END IF;
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        SELECT r.locked_at IS NOT NULL INTO locked
        FROM public.reads r WHERE r.id = OLD.read_id FOR UPDATE;
        IF locked IS TRUE THEN
            RAISE EXCEPTION 'read is locked'
                USING ERRCODE = '23514', CONSTRAINT = 'lc1_decision_read_locked';
        END IF;
        RETURN OLD;
    END IF;

    -- UPDATE: lock both distinct parents in ascending id order so two moves in
    -- opposite directions serialize, then reject if either parent is locked.
    PERFORM 1 FROM public.reads r
    WHERE r.id IN (OLD.read_id, NEW.read_id)
    ORDER BY r.id
    FOR UPDATE;
    IF EXISTS (
        SELECT 1 FROM public.reads r
        WHERE r.id IN (OLD.read_id, NEW.read_id) AND r.locked_at IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'read is locked'
            USING ERRCODE = '23514', CONSTRAINT = 'lc1_decision_read_locked';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER lc1_candidate_decisions_guard
    BEFORE INSERT OR UPDATE OR DELETE ON candidate_decisions
    FOR EACH ROW EXECUTE FUNCTION lc1_guard_decision_mutation();

-- +goose StatementBegin
CREATE FUNCTION lc1_guard_finalized_case() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    -- The initial_locked -> finalized transition is an ordinary update while OLD
    -- is not yet finalized; every other row the guard sees already had OLD.status
    -- finalized, so it must be frozen.
    IF OLD.status <> 'finalized' THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'finalized case is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'lc1_case_finalized_immutable';
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.source IS DISTINCT FROM OLD.source
        OR NEW.study_instance_uid IS DISTINCT FROM OLD.study_instance_uid
        OR NEW.series_instance_uid IS DISTINCT FROM OLD.series_instance_uid
        OR NEW.orthanc_series_id IS DISTINCT FROM OLD.orthanc_series_id
        OR NEW.frame_of_reference_uid IS DISTINCT FROM OLD.frame_of_reference_uid
        OR NEW.status IS DISTINCT FROM OLD.status
        OR NEW.finalized_at IS DISTINCT FROM OLD.finalized_at
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'finalized case is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'lc1_case_finalized_immutable';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER lc1_finalized_cases_guard
    BEFORE UPDATE OR DELETE ON cases
    FOR EACH ROW EXECUTE FUNCTION lc1_guard_finalized_case();

-- +goose Down
-- Removing this migration removes the decision/finalized-case protection only.
DROP TRIGGER IF EXISTS lc1_finalized_cases_guard ON cases;
DROP TRIGGER IF EXISTS lc1_candidate_decisions_guard ON candidate_decisions;
DROP FUNCTION IF EXISTS lc1_guard_finalized_case();
DROP FUNCTION IF EXISTS lc1_guard_decision_mutation();
