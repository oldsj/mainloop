-- +goose Up
-- Privileged actions, slice 3 (docs/specs/privileged-actions.md): the typed Kubernetes actions, run
-- by serve-executor. The executor's grants (00890) already cover them.
ALTER TABLE privileged_actions DROP CONSTRAINT privileged_actions_type_check;
ALTER TABLE privileged_actions ADD CONSTRAINT privileged_actions_type_check
    CHECK (type IN ('github.pr_merge', 'github.pr_close', 'onepassword.item_to_secret',
                    'k8s.rollout_restart', 'k8s.scale', 'k8s.delete'));

-- +goose Down
DELETE FROM privileged_actions WHERE type IN ('k8s.rollout_restart', 'k8s.scale', 'k8s.delete');
ALTER TABLE privileged_actions DROP CONSTRAINT privileged_actions_type_check;
ALTER TABLE privileged_actions ADD CONSTRAINT privileged_actions_type_check
    CHECK (type IN ('github.pr_merge', 'github.pr_close', 'onepassword.item_to_secret'));
