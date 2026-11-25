-- +goose Up
-- Privileged actions, slice 3b (docs/specs/privileged-actions.md): command.omnictl and
-- command.talosctl, run by serve-executor through Omni. The executor's grants (00890) already cover
-- them, as they do command.kubectl.
ALTER TABLE privileged_actions DROP CONSTRAINT privileged_actions_type_check;
ALTER TABLE privileged_actions ADD CONSTRAINT privileged_actions_type_check
    CHECK (type IN ('github.pr_merge', 'github.pr_close', 'onepassword.item_to_secret',
                    'k8s.rollout_restart', 'k8s.scale', 'k8s.delete', 'k8s.patch', 'k8s.apply',
                    'argocd.sync', 'command.kubectl', 'command.omnictl', 'command.talosctl'));

-- +goose Down
DELETE FROM privileged_actions WHERE type IN ('command.omnictl', 'command.talosctl');
ALTER TABLE privileged_actions DROP CONSTRAINT privileged_actions_type_check;
ALTER TABLE privileged_actions ADD CONSTRAINT privileged_actions_type_check
    CHECK (type IN ('github.pr_merge', 'github.pr_close', 'onepassword.item_to_secret',
                    'k8s.rollout_restart', 'k8s.scale', 'k8s.delete', 'k8s.patch', 'k8s.apply',
                    'argocd.sync', 'command.kubectl'));
