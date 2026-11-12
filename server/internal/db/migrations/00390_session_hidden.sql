-- +goose Up
-- A hidden session is one Mainloop runs for itself (the canary, docs/specs/canary.md). It works like
-- any other session but is left out of the owner's sessions list unless asked for with
-- GET /sessions?hidden=true. Set at create, never changed.
ALTER TABLE sessions ADD COLUMN hidden boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE sessions DROP COLUMN hidden;
