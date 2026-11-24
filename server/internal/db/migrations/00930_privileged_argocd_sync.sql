-- +goose Up
-- Privileged actions, slice 3 (docs/specs/privileged-actions.md): argocd.sync, run by
-- serve-executor. The executor's grants (00890) already cover it.
ALTER TABLE privileged_actions DROP CONSTRAINT privileged_actions_type_check;
ALTER TABLE privileged_actions ADD CONSTRAINT privileged_actions_type_check
    CHECK (type IN ('github.pr_merge', 'github.pr_close', 'onepassword.item_to_secret',
                    'k8s.rollout_restart', 'k8s.scale', 'k8s.delete', 'k8s.patch', 'k8s.apply',
                    'argocd.sync'));

-- +goose Down
DELETE FROM privileged_actions WHERE type = 'argocd.sync';
ALTER TABLE privileged_actions DROP CONSTRAINT privileged_actions_type_check;
ALTER TABLE privileged_actions ADD CONSTRAINT privileged_actions_type_check
    CHECK (type IN ('github.pr_merge', 'github.pr_close', 'onepassword.item_to_secret',
                    'k8s.rollout_restart', 'k8s.scale', 'k8s.delete', 'k8s.patch', 'k8s.apply'));
