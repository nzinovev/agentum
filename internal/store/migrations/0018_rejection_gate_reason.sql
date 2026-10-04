-- +goose Up
UPDATE runs AS run
SET cancel_reason = CASE WHEN EXISTS (
    SELECT 1 FROM run_approvals AS approval
    WHERE approval.tenant_id = run.tenant_id AND approval.user_id = run.user_id
      AND approval.run_id = run.id
      AND approval.name = 'final_review' AND approval.decision = 'rejected'
) THEN 'rejected_at_final_review' ELSE 'rejected_at_plan' END
WHERE run.state = 'cancelled' AND run.cancel_reason = 'rejected';

-- +goose Down
UPDATE runs
SET cancel_reason = 'rejected'
WHERE cancel_reason IN ('rejected_at_plan', 'rejected_at_final_review');
