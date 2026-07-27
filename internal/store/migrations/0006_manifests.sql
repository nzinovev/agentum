-- +goose Up
-- +goose StatementBegin

-- F.7 — evidence manifest per run.
--
-- The manifest records everything that went into a single task run: the input,
-- the pack and its resolved version/hash, the prompt revisions the adapter saw,
-- the model + tier, the effective capability profile, the memory slice pulled,
-- the input/output artifact revisions, the checks-version + their results, the
-- human gate decisions, and the git lineage (branch, checkpoints, base /
-- result commit).
--
-- Lifecycle:
--   - InitManifest creates one row per task at task creation. UNIQUE (task_id)
--     enforces "one manifest per task."
--   - AddEvidence merges keys into body. Once a section is filled it is not
--     overwritten — append-only by convention; the AddEvidence query enforces
--     it by refusing to write when sealed_at IS NOT NULL.
--   - Seal sets sealed_at + seal_reason. After sealing, AddEvidence is rejected
--     with a typed error; corrections are a separate linked row
--     (task_manifest_corrections) that supersedes the sealed body.
--
-- Subsystems not yet implemented (project memory, capability enforcement,
-- project checks) are recorded as explicit `missing` entries in the body — the
-- manifest never hides an absent input, it surfaces it.

CREATE TABLE task_manifests (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL,
    user_id      uuid NOT NULL,
    task_id      uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,

    -- The manifest body. A jsonb blob whose shape is owned by
    -- internal/manifest. Sections are added as evidence accumulates; the body
    -- is never rewritten wholesale.
    body         jsonb NOT NULL DEFAULT '{}',

    -- Seal. NULL while evidence is accumulating; set once the task reaches a
    -- terminal state (done | failed | cancelled) or is interrupted. After
    -- sealing the body is immutable; corrections land in
    -- task_manifest_corrections as linked revisions.
    sealed_at    timestamptz,
    sealed_by    uuid,                                 -- the user/system that sealed
    seal_reason  text,                                 -- completed | interrupted | cancelled | failed

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    UNIQUE (task_id)
);

CREATE INDEX idx_manifests_tenant ON task_manifests(tenant_id);
CREATE INDEX idx_manifests_sealed ON task_manifests(sealed_at) WHERE sealed_at IS NOT NULL;

-- task_manifest_corrections: post-seal amendments as linked revisions. A
-- correction never edits the sealed manifest — it adds a new row whose body
-- carries the corrected evidence, with a `reason` explaining why. Consumers
-- (UI, comparison) read the sealed manifest joined with its corrections in
-- created_at order.
CREATE TABLE task_manifest_corrections (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL,
    user_id      uuid NOT NULL,
    manifest_id  uuid NOT NULL REFERENCES task_manifests(id) ON DELETE CASCADE,

    -- The corrected body (full replacement shape; the diff against the sealed
    -- body is what changed). Stored as a snapshot so corrections are
    -- self-contained and do not depend on the sealed row staying put.
    body         jsonb NOT NULL,
    reason       text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_manifest_corrections_manifest
    ON task_manifest_corrections(manifest_id, created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS task_manifest_corrections;
DROP TABLE IF EXISTS task_manifests;

-- +goose StatementEnd
