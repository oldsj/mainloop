-- +goose Up
ALTER TABLE task_attempts ADD COLUMN model text
    CHECK (model IS NULL OR model ~ '^[A-Za-z0-9._:/-]{1,200}$');
ALTER TABLE task_attempts ADD COLUMN effort text
    CHECK (effort IS NULL OR effort ~ '^[a-z]{1,20}$');

-- +goose Down
ALTER TABLE task_attempts DROP COLUMN effort;
ALTER TABLE task_attempts DROP COLUMN model;
