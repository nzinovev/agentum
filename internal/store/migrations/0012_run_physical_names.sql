-- +goose Up
-- +goose StatementBegin

-- The physical names of the launch entity move to "run": five tables, eight
-- columns, nineteen constraints, eight indexes. The vocabulary decision made
-- the human-facing surfaces say "run" earlier; this change makes the schema and
-- the Go identifiers say the same, so no layer keeps a second word for the one
-- entity that executes a pipeline.
--
-- The rename is done now, before the launch entity is ever split into a run +
-- work item, for one reason: the failure mode. After this migration no relation
-- carries the legacy name, so any reference the rename missed is a loud
-- failure — a missing-relation error from Postgres or a compile error in Go.
-- Doing the same edit together with the split would make the old name point at
-- a NEW table with a different meaning, and a missed reference would compile
-- and execute against the wrong table.
--
-- Order matters and is verified behavior, not assumption: RENAME TABLE moves
-- neither constraints nor indexes, and they keep their old names until renamed
-- explicitly. A constraint RENAME on a PRIMARY KEY or UNIQUE constraint renames
-- its backing index too (they share one name); plain indexes need ALTER INDEX.
-- So: tables first, then columns, then constraints and indexes addressed by
-- their new table names.

-- 1. Tables.
ALTER TABLE tasks                     RENAME TO runs;
ALTER TABLE task_approvals            RENAME TO run_approvals;
ALTER TABLE task_checkpoints          RENAME TO run_checkpoints;
ALTER TABLE task_manifests            RENAME TO run_manifests;
ALTER TABLE task_manifest_corrections RENAME TO run_manifest_corrections;

-- 2. Columns.
ALTER TABLE stage_invocations RENAME COLUMN task_id TO run_id;
ALTER TABLE events            RENAME COLUMN task_id TO run_id;
ALTER TABLE jobs              RENAME COLUMN task_id TO run_id;
ALTER TABLE artifact_revisions RENAME COLUMN task_id TO run_id;
ALTER TABLE run_checkpoints   RENAME COLUMN task_id TO run_id;
ALTER TABLE run_approvals     RENAME COLUMN task_id TO run_id;
ALTER TABLE run_manifests     RENAME COLUMN task_id TO run_id;
ALTER TABLE memory_entries    RENAME COLUMN source_task_id TO source_run_id;

-- 3. Constraints (PK and UNIQUE renames carry their backing indexes along).
ALTER TABLE runs RENAME CONSTRAINT tasks_pkey TO runs_pkey;
ALTER TABLE runs RENAME CONSTRAINT tasks_project_fk TO runs_project_fk;
ALTER TABLE run_approvals RENAME CONSTRAINT task_approvals_pkey TO run_approvals_pkey;
ALTER TABLE run_approvals RENAME CONSTRAINT task_approvals_actor_kind TO run_approvals_actor_kind;
ALTER TABLE run_approvals RENAME CONSTRAINT task_approvals_task_id_fkey TO run_approvals_run_id_fkey;
ALTER TABLE run_approvals RENAME CONSTRAINT task_approvals_artifact_revision_id_fkey TO run_approvals_artifact_revision_id_fkey;
ALTER TABLE run_checkpoints RENAME CONSTRAINT task_checkpoints_pkey TO run_checkpoints_pkey;
ALTER TABLE run_checkpoints RENAME CONSTRAINT task_checkpoints_task_id_fkey TO run_checkpoints_run_id_fkey;
ALTER TABLE run_checkpoints RENAME CONSTRAINT task_checkpoints_task_id_label_key TO run_checkpoints_run_id_label_key;
ALTER TABLE run_manifests RENAME CONSTRAINT task_manifests_pkey TO run_manifests_pkey;
ALTER TABLE run_manifests RENAME CONSTRAINT task_manifests_task_id_fkey TO run_manifests_run_id_fkey;
ALTER TABLE run_manifests RENAME CONSTRAINT task_manifests_task_id_key TO run_manifests_run_id_key;
ALTER TABLE run_manifest_corrections RENAME CONSTRAINT task_manifest_corrections_pkey TO run_manifest_corrections_pkey;
ALTER TABLE run_manifest_corrections RENAME CONSTRAINT task_manifest_corrections_manifest_id_fkey TO run_manifest_corrections_manifest_id_fkey;
ALTER TABLE artifact_revisions RENAME CONSTRAINT artifact_revisions_task_id_fkey TO artifact_revisions_run_id_fkey;
ALTER TABLE events RENAME CONSTRAINT events_task_id_fkey TO events_run_id_fkey;
ALTER TABLE jobs RENAME CONSTRAINT jobs_task_id_fkey TO jobs_run_id_fkey;
ALTER TABLE memory_entries RENAME CONSTRAINT memory_entries_source_task_id_fkey TO memory_entries_source_run_id_fkey;
ALTER TABLE stage_invocations RENAME CONSTRAINT stage_invocations_task_id_fkey TO stage_invocations_run_id_fkey;

-- 4. Plain indexes.
ALTER INDEX idx_tasks_state RENAME TO idx_runs_state;
ALTER INDEX idx_tasks_tenant_project RENAME TO idx_runs_tenant_project;
ALTER INDEX idx_events_task RENAME TO idx_events_run;
ALTER INDEX idx_stages_task RENAME TO idx_stages_run;
ALTER INDEX idx_checkpoints_task RENAME TO idx_checkpoints_run;
ALTER INDEX idx_artifact_rev_task_name RENAME TO idx_artifact_rev_run_name;
ALTER INDEX idx_task_approvals_task RENAME TO idx_run_approvals_run;
ALTER INDEX idx_task_approvals_unique RENAME TO idx_run_approvals_unique;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Symmetric renames in reverse order: indexes, constraints, columns, tables.

ALTER INDEX idx_run_approvals_unique RENAME TO idx_task_approvals_unique;
ALTER INDEX idx_run_approvals_run RENAME TO idx_task_approvals_task;
ALTER INDEX idx_artifact_rev_run_name RENAME TO idx_artifact_rev_task_name;
ALTER INDEX idx_checkpoints_run RENAME TO idx_checkpoints_task;
ALTER INDEX idx_stages_run RENAME TO idx_stages_task;
ALTER INDEX idx_events_run RENAME TO idx_events_task;
ALTER INDEX idx_runs_tenant_project RENAME TO idx_tasks_tenant_project;
ALTER INDEX idx_runs_state RENAME TO idx_tasks_state;

ALTER TABLE stage_invocations RENAME CONSTRAINT stage_invocations_run_id_fkey TO stage_invocations_task_id_fkey;
ALTER TABLE memory_entries RENAME CONSTRAINT memory_entries_source_run_id_fkey TO memory_entries_source_task_id_fkey;
ALTER TABLE jobs RENAME CONSTRAINT jobs_run_id_fkey TO jobs_task_id_fkey;
ALTER TABLE events RENAME CONSTRAINT events_run_id_fkey TO events_task_id_fkey;
ALTER TABLE artifact_revisions RENAME CONSTRAINT artifact_revisions_run_id_fkey TO artifact_revisions_task_id_fkey;
ALTER TABLE run_manifest_corrections RENAME CONSTRAINT run_manifest_corrections_manifest_id_fkey TO task_manifest_corrections_manifest_id_fkey;
ALTER TABLE run_manifest_corrections RENAME CONSTRAINT run_manifest_corrections_pkey TO task_manifest_corrections_pkey;
ALTER TABLE run_manifests RENAME CONSTRAINT run_manifests_run_id_key TO task_manifests_task_id_key;
ALTER TABLE run_manifests RENAME CONSTRAINT run_manifests_run_id_fkey TO task_manifests_task_id_fkey;
ALTER TABLE run_manifests RENAME CONSTRAINT run_manifests_pkey TO task_manifests_pkey;
ALTER TABLE run_checkpoints RENAME CONSTRAINT run_checkpoints_run_id_label_key TO task_checkpoints_task_id_label_key;
ALTER TABLE run_checkpoints RENAME CONSTRAINT run_checkpoints_run_id_fkey TO task_checkpoints_task_id_fkey;
ALTER TABLE run_checkpoints RENAME CONSTRAINT run_checkpoints_pkey TO task_checkpoints_pkey;
ALTER TABLE run_approvals RENAME CONSTRAINT run_approvals_artifact_revision_id_fkey TO task_approvals_artifact_revision_id_fkey;
ALTER TABLE run_approvals RENAME CONSTRAINT run_approvals_run_id_fkey TO task_approvals_task_id_fkey;
ALTER TABLE run_approvals RENAME CONSTRAINT run_approvals_actor_kind TO task_approvals_actor_kind;
ALTER TABLE run_approvals RENAME CONSTRAINT run_approvals_pkey TO task_approvals_pkey;
ALTER TABLE runs RENAME CONSTRAINT runs_project_fk TO tasks_project_fk;
ALTER TABLE runs RENAME CONSTRAINT runs_pkey TO tasks_pkey;

ALTER TABLE memory_entries RENAME COLUMN source_run_id TO source_task_id;
ALTER TABLE run_manifests RENAME COLUMN run_id TO task_id;
ALTER TABLE run_approvals RENAME COLUMN run_id TO task_id;
ALTER TABLE run_checkpoints RENAME COLUMN run_id TO task_id;
ALTER TABLE artifact_revisions RENAME COLUMN run_id TO task_id;
ALTER TABLE jobs RENAME COLUMN run_id TO task_id;
ALTER TABLE events RENAME COLUMN run_id TO task_id;
ALTER TABLE stage_invocations RENAME COLUMN run_id TO task_id;

ALTER TABLE run_manifest_corrections RENAME TO task_manifest_corrections;
ALTER TABLE run_manifests RENAME TO task_manifests;
ALTER TABLE run_checkpoints RENAME TO task_checkpoints;
ALTER TABLE run_approvals RENAME TO task_approvals;
ALTER TABLE runs RENAME TO tasks;

-- +goose StatementEnd
