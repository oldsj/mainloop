-- +goose Up
-- Privileged actions: command.exec (docs/specs/privileged-actions.md), a script run by
-- serve-executor in a single-use toolbox pod. The executor's grants (00890) already cover it.
ALTER TABLE privileged_actions DROP CONSTRAINT privileged_actions_type_check;
ALTER TABLE privileged_actions ADD CONSTRAINT privileged_actions_type_check
    CHECK (type IN ('github.pr_merge', 'github.pr_close', 'onepassword.item_to_secret',
                    'k8s.rollout_restart', 'k8s.scale', 'k8s.delete', 'k8s.patch', 'k8s.apply',
                    'argocd.sync', 'command.kubectl', 'command.omnictl', 'command.talosctl',
                    'command.ssh', 'http.unifi', 'command.exec'));

-- +goose Down
DELETE FROM privileged_actions WHERE type = 'command.exec';
ALTER TABLE privileged_actions DROP CONSTRAINT privileged_actions_type_check;
ALTER TABLE privileged_actions ADD CONSTRAINT privileged_actions_type_check
    CHECK (type IN ('github.pr_merge', 'github.pr_close', 'onepassword.item_to_secret',
                    'k8s.rollout_restart', 'k8s.scale', 'k8s.delete', 'k8s.patch', 'k8s.apply',
                    'argocd.sync', 'command.kubectl', 'command.omnictl', 'command.talosctl',
                    'command.ssh', 'http.unifi'));
