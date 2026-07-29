-- +goose Up
-- +goose StatementBegin

-- Epic 6 — code-enforced capability profiles.
--
-- Each agent invocation must carry its effective capability profile as saved
-- audit evidence (the requirement: "each invocation must have a saved effective
-- profile"). The capability_profile column is the authoritative per-invocation
-- record; the manifest body mirrors a snapshot for cross-run diffing. The
-- column is nullable: rows from before this migration and invocations that
-- could not start (e.g. an unenforceable profile) leave it NULL rather than
-- synthesizing an empty profile that could be misread as "granted nothing and
-- that was fine."

ALTER TABLE stage_invocations ADD COLUMN capability_profile jsonb;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE stage_invocations DROP COLUMN IF EXISTS capability_profile;

-- +goose StatementEnd
