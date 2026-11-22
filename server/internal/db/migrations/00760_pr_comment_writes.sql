-- +goose Up
-- Pull request comments (internal/issues, docs/specs/issue-writes.md): comment_pull_request writes
-- an issue_writes row like comment_issue does, and the same loop posts it at most once.
--
--   kind pr_comment  a conversation comment on a pull request; issue_number is the pull request
--   kind pr_review   a review with inline comments, always the COMMENT event, never an approval or a
--                    change request; commit_sha is the commit its lines refer to, review_comments
--                    the inline comments as sent ([{path, line, side, body}]), and comment_id, once
--                    created, the review's id
ALTER TABLE issue_writes DROP CONSTRAINT issue_writes_kind_check;
ALTER TABLE issue_writes ADD CONSTRAINT issue_writes_kind_check
CHECK (kind IN ('issue', 'comment', 'pr_comment', 'pr_review'));
ALTER TABLE issue_writes ADD COLUMN commit_sha text;
ALTER TABLE issue_writes ADD COLUMN review_comments jsonb NOT NULL DEFAULT '[]';
ALTER TABLE issue_writes ADD CONSTRAINT issue_writes_review_check CHECK (
    CASE
        WHEN kind = 'pr_review' THEN
            commit_sha ~ '^[0-9a-f]{40}$'
            AND jsonb_typeof(review_comments) = 'array'
            AND jsonb_array_length(review_comments) > 0
        ELSE commit_sha IS NULL AND review_comments = '[]'::jsonb
    END
);

-- +goose Down
DELETE FROM issue_writes WHERE kind IN ('pr_comment', 'pr_review');
ALTER TABLE issue_writes DROP CONSTRAINT issue_writes_review_check;
ALTER TABLE issue_writes DROP COLUMN review_comments;
ALTER TABLE issue_writes DROP COLUMN commit_sha;
ALTER TABLE issue_writes DROP CONSTRAINT issue_writes_kind_check;
ALTER TABLE issue_writes ADD CONSTRAINT issue_writes_kind_check CHECK (kind IN ('issue', 'comment'));
