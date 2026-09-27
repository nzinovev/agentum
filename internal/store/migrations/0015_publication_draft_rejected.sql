-- +goose Up
ALTER TABLE run_publications ADD COLUMN draft_rejected boolean NOT NULL DEFAULT false;

UPDATE run_publications SET draft_rejected = true
WHERE last_error_code = 'draft_unsupported' AND pr_number IS NOT NULL;

-- +goose Down
ALTER TABLE run_publications DROP COLUMN draft_rejected;
