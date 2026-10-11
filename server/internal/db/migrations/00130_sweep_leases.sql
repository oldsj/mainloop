-- +goose Up
-- One row per reconcile sweep that is running right now. A sweep takes its row in a short
-- transaction, extends it while it runs, and deletes it when it ends; a holder that dies stops
-- extending, so `until` lapses and another process takes the sweep over. This replaces a
-- session-scoped advisory lock, which pinned a pool connection for the whole sweep, including its
-- kagent, GitHub and Kubernetes calls.
--
-- `holder` is unique per run (the runner's owner name plus a counter), so a run that outlived its
-- lease cannot extend or delete the row of the run that replaced it. A sweep that has never run,
-- or has finished, has no row. New file, not an edit in place: dev databases already applied
-- the earlier files.
CREATE TABLE sweep_leases (
    name text PRIMARY KEY,
    holder text NOT NULL,
    until timestamptz NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS sweep_leases;
