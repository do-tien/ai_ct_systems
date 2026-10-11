-- +goose Up
-- Read and mark immutability guards (LC1-11).
--
-- The guards are a data-integrity boundary, not a privilege system: the owner
-- can still drop them. They make a locked initial read and its marks immutable
-- through application code, direct runtime SQL and mark reassignment.
-- +goose StatementBegin
CREATE FUNCTION lc1_guard_read_mutation() RETURNS trigger
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

CREATE TRIGGER lc1_reads_guard
    BEFORE UPDATE OR DELETE ON reads
    FOR EACH ROW EXECUTE FUNCTION lc1_guard_read_mutation();

-- +goose StatementBegin
CREATE FUNCTION lc1_guard_mark_mutation() RETURNS trigger
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
                USING ERRCODE = '23514', CONSTRAINT = 'lc1_mark_read_locked';
        END IF;
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        SELECT r.locked_at IS NOT NULL INTO locked
        FROM public.reads r WHERE r.id = OLD.read_id FOR UPDATE;
        IF locked IS TRUE THEN
            RAISE EXCEPTION 'read is locked'
                USING ERRCODE = '23514', CONSTRAINT = 'lc1_mark_read_locked';
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
            USING ERRCODE = '23514', CONSTRAINT = 'lc1_mark_read_locked';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER lc1_marks_guard
    BEFORE INSERT OR UPDATE OR DELETE ON marks
    FOR EACH ROW EXECUTE FUNCTION lc1_guard_mark_mutation();

-- +goose Down
-- Removing this migration removes the read/mark immutability protection.
DROP TRIGGER IF EXISTS lc1_marks_guard ON marks;
DROP TRIGGER IF EXISTS lc1_reads_guard ON reads;
DROP FUNCTION IF EXISTS lc1_guard_mark_mutation();
DROP FUNCTION IF EXISTS lc1_guard_read_mutation();
