-- +goose Up
-- Task 13 review (S58): a write held for the person's decision stays
-- counted against its turn's limit of 3 after it is decided, so the row
-- records that it was held.
alter table memory_writes add column held boolean not null default false;

-- +goose Down
alter table memory_writes drop column held;
