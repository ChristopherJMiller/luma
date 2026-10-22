-- +goose Up
-- Task 14 review: (turn_id, model) identifies a ledger row. A turn's rows are
-- written in one transaction with ON CONFLICT DO NOTHING on this key, so a
-- turn_result delivered twice, or an eval run posted twice (its turn id is
-- eval_<suite>_<finished_at>), is counted once.
create unique index cost_ledger_turn_model_key on cost_ledger (turn_id, model);

-- +goose Down
drop index cost_ledger_turn_model_key;
