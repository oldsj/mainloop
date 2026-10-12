-- +goose Up
-- The merge proposal that settled a code task's pull request. The completion sweep sets it in the
-- statement that completes the task: a task whose own pull request the merge service merged.
-- merge_state in the task projection is derived from the observed pull request state.
ALTER TABLE tasks ADD COLUMN merge_proposal_id text REFERENCES merge_proposals (id);
ALTER TABLE tasks ADD CONSTRAINT tasks_merge_proposal_completed CHECK (merge_proposal_id IS NULL OR status = 'completed');

-- +goose Down
ALTER TABLE tasks DROP CONSTRAINT tasks_merge_proposal_completed;
ALTER TABLE tasks DROP COLUMN merge_proposal_id;
