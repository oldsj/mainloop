-- +goose Up
-- Privileged actions, slice 3 (docs/specs/privileged-actions.md): command.kubectl, run by
-- serve-executor as cluster-admin. The executor's grants (00890) already cover it: it extends its
-- own claim through claimed_until, which it may update.
ALTER TABLE privileged_actions DROP CONSTRAINT privileged_actions_type_check;
ALTER TABLE privileged_actions ADD CONSTRAINT privileged_actions_type_check
    CHECK (type IN ('github.pr_merge', 'github.pr_close', 'onepassword.item_to_secret',
                    'k8s.rollout_restart', 'k8s.scale', 'k8s.delete', 'k8s.patch', 'k8s.apply',
                    'argocd.sync', 'command.kubectl'));

-- +goose Down
DELETE FROM privileged_actions WHERE type = 'command.kubectl';
ALTER TABLE privileged_actions DROP CONSTRAINT privileged_actions_type_check;
ALTER TABLE privileged_actions ADD CONSTRAINT privileged_actions_type_check
    CHECK (type IN ('github.pr_merge', 'github.pr_close', 'onepassword.item_to_secret',
                    'k8s.rollout_restart', 'k8s.scale', 'k8s.delete', 'k8s.patch', 'k8s.apply',
                    'argocd.sync'));
