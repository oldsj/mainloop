-- +goose Up
-- vcluster_server overrides the server named by a vcluster target's own kubeconfig Secret (read
-- through devloop.VClusterPods), for a vcluster whose exportKubeConfig.server only resolves from
-- inside its own pod (e.g. https://localhost:8443). Optional even for a vcluster target: the infra
-- side should set the vcluster's exportKubeConfig.server to its in-cluster Service URL instead,
-- which makes this override unnecessary in the common case. See docs/specs/devloop.md.
ALTER TABLE devloop_targets ADD COLUMN vcluster_server text
    CHECK (vcluster_server IS NULL OR kind = 'vcluster');

-- +goose Down
ALTER TABLE devloop_targets DROP COLUMN IF EXISTS vcluster_server;
