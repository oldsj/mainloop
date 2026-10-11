-- +goose Up
-- Freeze the authority behind a push grant. A changed writer or native runtime requires a new
-- workspace; a credential rotation cannot silently transfer publication authority. Empty defaults
-- preserve incomplete audit/fixture rows, which cannot authenticate or resolve as real pushes.
ALTER TABLE git_push_grants
    ADD COLUMN owner_id text NOT NULL DEFAULT '',
    ADD COLUMN writer_generation integer NOT NULL DEFAULT 0,
    ADD COLUMN attempt_id text,
    ADD COLUMN native_session_id text NOT NULL DEFAULT '',
    ADD COLUMN runtime jsonb NOT NULL DEFAULT '{}';
ALTER TABLE git_publications ADD COLUMN owner_id text NOT NULL DEFAULT '';
-- Creation-time push attachment is an intent, keyed by the task attempt. Keep the identity after
-- workspace deletion so a repeated attempt cannot reserve a different reference or workspace.
CREATE TABLE workspace_push_reservations (
    id text PRIMARY KEY,
    idempotency_key text NOT NULL UNIQUE,
    workspace_id text NOT NULL UNIQUE,
    attempt_id text NOT NULL UNIQUE,
    reference jsonb NOT NULL,
    state text NOT NULL DEFAULT 'planned' CHECK (state IN ('planned', 'published', 'revoked')),
    expires_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX workspace_push_reservations_claim ON workspace_push_reservations (next_attempt_at, id)
WHERE state = 'planned';
-- +goose StatementBegin
CREATE FUNCTION workspace_push_reservation_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.id, OLD.idempotency_key, OLD.workspace_id, OLD.attempt_id, OLD.reference)
       IS DISTINCT FROM ROW(NEW.id, NEW.idempotency_key, NEW.workspace_id, NEW.attempt_id, NEW.reference)
       OR (OLD.state <> NEW.state AND NOT (
           (OLD.state = 'planned' AND NEW.state IN ('published', 'revoked')) OR
           (OLD.state = 'published' AND NEW.state = 'revoked'))) THEN
        RAISE EXCEPTION 'push reservation identity or terminal state immutable';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER workspace_push_reservation_identity BEFORE UPDATE ON workspace_push_reservations
FOR EACH ROW EXECUTE FUNCTION workspace_push_reservation_identity();
-- Admission precedes reading a body, including the bounded probe. Expired rows can be replaced
-- after a crashed handler; request IDs prevent its eventual cleanup from releasing a successor.
CREATE TABLE git_push_admissions (
    repository text NOT NULL,
    branch text NOT NULL,
    id text NOT NULL UNIQUE,
    lease_until timestamptz NOT NULL,
    PRIMARY KEY (repository, branch)
);
-- Pending rows also fence the branch: they are validated intents, never a queue of push bodies.
DROP INDEX git_publications_unresolved;
CREATE UNIQUE INDEX git_publications_unresolved ON git_publications (owner_id, repository, branch)
WHERE owner_id <> '' AND state IN ('pending', 'dispatching', 'unknown');
CREATE INDEX git_publications_confirmed_body ON git_publications
    (workspace_id, (attempt->>'credential_id'), (attempt->>'body_digest')) WHERE state = 'confirmed';

-- Serialize intent admission with G8b writer release/transfer. The HTTP handler locks this same
-- claim in a short transaction, then commits the fence before any external publication bytes.
-- +goose StatementBegin
CREATE FUNCTION pushgate_writer_fence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM git_publications
        WHERE (owner_id = OLD.owner_id OR owner_id = '') AND repository = OLD.repository AND branch = OLD.branch
          AND state IN ('pending', 'dispatching', 'unknown')) OR EXISTS (
        SELECT 1 FROM git_push_admissions WHERE repository = OLD.repository AND branch = OLD.branch
          AND lease_until > clock_timestamp()) THEN
        RAISE EXCEPTION 'branch publication unresolved';
    END IF;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER pushgate_writer_fence BEFORE UPDATE OR DELETE ON workspace_writer_claims
FOR EACH ROW EXECUTE FUNCTION pushgate_writer_fence();

-- +goose StatementBegin
CREATE FUNCTION pushgate_grant_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.workspace_id, OLD.branch, OLD.policy_version, OLD.owner_id, OLD.writer_generation,
           OLD.attempt_id, OLD.native_session_id, OLD.runtime)
       IS DISTINCT FROM
       ROW(NEW.workspace_id, NEW.branch, NEW.policy_version, NEW.owner_id, NEW.writer_generation,
           NEW.attempt_id, NEW.native_session_id, NEW.runtime)
       OR (OLD.credential_id IS NOT NULL AND OLD.credential_id IS DISTINCT FROM NEW.credential_id)
       OR OLD.state = 'revoked' THEN
        RAISE EXCEPTION 'push grant identity immutable';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER pushgate_grant_identity BEFORE UPDATE ON git_push_grants
FOR EACH ROW EXECUTE FUNCTION pushgate_grant_identity();

-- +goose StatementBegin
CREATE FUNCTION pushgate_publication_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.owner_id = '' THEN RETURN NEW; END IF;
    IF ROW(OLD.id, OLD.workspace_id, OLD.request_id, OLD.owner_id, OLD.repository, OLD.branch, OLD.attempt)
       IS DISTINCT FROM ROW(NEW.id, NEW.workspace_id, NEW.request_id, NEW.owner_id, NEW.repository, NEW.branch, NEW.attempt) THEN
        RAISE EXCEPTION 'publication identity immutable';
    END IF;
    IF OLD.state <> NEW.state AND NOT (
        (OLD.state = 'pending' AND NEW.state IN ('dispatching', 'rejected', 'unknown')) OR
        (OLD.state = 'dispatching' AND NEW.state IN ('confirmed', 'rejected', 'unknown')) OR
        (OLD.state = 'unknown' AND NEW.state IN ('confirmed', 'rejected'))
    ) THEN
        RAISE EXCEPTION 'publication state cannot reopen';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER pushgate_publication_identity BEFORE UPDATE ON git_publications
FOR EACH ROW EXECUTE FUNCTION pushgate_publication_identity();

-- +goose Down
DROP TABLE workspace_push_reservations;
DROP FUNCTION workspace_push_reservation_identity();
DROP TRIGGER pushgate_publication_identity ON git_publications;
DROP FUNCTION pushgate_publication_identity();
DROP INDEX git_publications_confirmed_body;
DROP TRIGGER pushgate_grant_identity ON git_push_grants;
DROP FUNCTION pushgate_grant_identity();
DROP TRIGGER pushgate_writer_fence ON workspace_writer_claims;
DROP FUNCTION pushgate_writer_fence();
DROP TABLE git_push_admissions;
DROP INDEX git_publications_unresolved;
CREATE INDEX git_publications_unresolved ON git_publications (repository, branch)
WHERE state IN ('dispatching', 'unknown');
ALTER TABLE git_publications DROP COLUMN owner_id;
ALTER TABLE git_push_grants DROP COLUMN owner_id, DROP COLUMN writer_generation,
    DROP COLUMN attempt_id, DROP COLUMN native_session_id, DROP COLUMN runtime;
