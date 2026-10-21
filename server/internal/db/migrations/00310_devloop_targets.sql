-- +goose Up
-- devloop_targets: a project's staging target, Mainloop-side configuration (never a file in the
-- project's own repository, since the owner works on arbitrary and third-party repos). One row per
-- project. The devloop lease and sync lock become per target (keyed by devloop_targets.id) instead
-- of the single "mainloop-dev" row; this migration seeds oldsj/mainloop's target with that id so
-- its lease and sync lock rows (already present) keep working unchanged. See docs/specs/devloop.md.

CREATE TABLE devloop_targets (
    id text PRIMARY KEY,
    project_id text NOT NULL UNIQUE REFERENCES projects (id) ON DELETE CASCADE,
    -- "namespace": namespace is the plain Kubernetes namespace the workload runs in.
    -- "vcluster": namespace is the *host* namespace holding the vcluster; vcluster_name and
    -- vcluster_namespace locate the workload inside the vcluster's own API server.
    kind text NOT NULL CHECK (kind IN ('namespace', 'vcluster')),
    namespace text NOT NULL,
    vcluster_name text,
    vcluster_namespace text,
    CHECK ((kind = 'vcluster') = (vcluster_name IS NOT NULL AND vcluster_namespace IS NOT NULL)),
    -- workload is a label selector (e.g. "app=mainloop-backend") identifying the pod that
    -- restart_cmd, migrate_cmd, health_cmd and smoke_cmd run in, inside command_container.
    workload text NOT NULL,
    command_container text NOT NULL,
    -- sync_paths is a JSON array of {selector, container, prefix, dest, exclude: []}, one entry per
    -- container a sync copies into: which repository subtree (prefix), which pod (selector) and
    -- container, where it lands (dest), and which paths under prefix it never copies or deletes.
    sync_paths jsonb NOT NULL DEFAULT '[]',
    -- Each optional: a target without one skips that step rather than failing it. Plain shell
    -- commands, run in command_container; language- and project-agnostic by construction.
    restart_cmd text,
    migrate_cmd text,
    health_cmd text,
    smoke_cmd text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- oldsj/mainloop's target, expressed in this table: the same namespace, pods, sync paths and
-- commands the hand-written mainloop-dev sync already used, so dev_sync keeps behaving exactly as
-- it does today. Conditional on the project already existing (it does in every environment that
-- has run dev_sync against mainloop-dev; a fresh database with no such project seeds nothing, which
-- is fine, since nothing references the target row until a project imports it).
INSERT INTO devloop_targets (
    id, project_id, kind, namespace, workload, command_container, sync_paths,
    migrate_cmd, health_cmd, smoke_cmd
)
SELECT
    'mainloop-dev', p.id, 'namespace', 'mainloop-dev', 'app=mainloop-backend', 'backend',
    '[
        {"selector": "app=mainloop-backend", "container": "backend", "prefix": "server", "dest": "/src",
         "exclude": [".git/", "bin/", "tmp/", "testdata/", "*.test", "*_test.go"]},
        {"selector": "app=mainloop-backend", "container": "mcp", "prefix": "server", "dest": "/src",
         "exclude": [".git/", "bin/", "tmp/", "testdata/", "*.test", "*_test.go"]},
        {"selector": "app=mainloop-backend", "container": "git", "prefix": "server", "dest": "/src",
         "exclude": [".git/", "bin/", "tmp/", "testdata/", "*.test", "*_test.go"]},
        {"selector": "app=mainloop-frontend", "container": "frontend", "prefix": "web", "dest": "/app/web",
         "exclude": ["node_modules/", "dist/", "e2e/", "playwright-report/", "test-results/"]}
    ]'::jsonb,
    'go run ./cmd/mainloop migrate up',
    'curl -fsS --max-time 3 -o /dev/null http://127.0.0.1:8000/health'
        || ' && curl -fsS --max-time 3 -o /dev/null http://127.0.0.1:8002/health'
        || ' && curl -fsS --max-time 3 -o /dev/null http://127.0.0.1:8003/health',
    'go run ./cmd/mainloop smoke --read-only --base-url http://127.0.0.1:8000 --mcp-url http://127.0.0.1:8002'
        || ' --git-url http://127.0.0.1:8003 --git-push-url http://127.0.0.1:8004'
FROM projects p
WHERE lower(p.full_name) = 'oldsj/mainloop'
ORDER BY p.created_at
LIMIT 1
ON CONFLICT (id) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS devloop_targets;
