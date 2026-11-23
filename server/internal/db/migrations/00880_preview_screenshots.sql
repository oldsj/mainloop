-- +goose Up
-- Screenshots of a workspace preview that Mainloop's headless browser took for the workspace's
-- agent (preview_screenshot, docs/specs/workspaces.md "Preview screenshots", issue #249). One row
-- per viewport of one request; a retry of the request returns the rows it already stored.
CREATE TABLE preview_screenshots (
    id text PRIMARY KEY,
    owner_id text NOT NULL,
    -- The workspace (its session) whose preview was captured, which is also the agent's binding.
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    request_id text NOT NULL CHECK (char_length(request_id) BETWEEN 1 AND 100),
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    path text NOT NULL CHECK (char_length(path) BETWEEN 1 AND 2048),
    viewport text NOT NULL CHECK (viewport IN ('mobile', 'desktop')),
    full_page boolean NOT NULL DEFAULT false,
    media_type text NOT NULL CHECK (media_type = 'image/png'),
    width integer NOT NULL CHECK (width > 0),
    height integer NOT NULL CHECK (height > 0),
    -- The HTTP status of the captured page's document; NULL when the browser reported none.
    page_status integer CHECK (page_status IS NULL OR page_status BETWEEN 100 AND 599),
    data bytea NOT NULL,
    size_bytes integer NOT NULL CHECK (size_bytes BETWEEN 1 AND 5242880),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (session_id, request_id, viewport)
);
-- The rate limit counts a session's recent captures.
CREATE INDEX preview_screenshots_session_created ON preview_screenshots (session_id, created_at);

-- A report names the screenshots it shows the owner; the task view and the owner's conversation
-- render them with it.
ALTER TABLE task_reports ADD COLUMN screenshot_ids text[] NOT NULL DEFAULT '{}'
    CHECK (cardinality(screenshot_ids) <= 8);

-- +goose Down
ALTER TABLE task_reports DROP COLUMN screenshot_ids;
DROP TABLE preview_screenshots;
