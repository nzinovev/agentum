-- +goose Up
-- +goose StatementBegin

-- F.7 — immutable artifact revisions and evidence manifest.
--
-- The worktree is disposable; the artifacts an agent produces during a stage
-- are the durable output of a run. Until this migration those artifacts lived
-- only inside the per-task worktree and were discarded at teardown (only the
-- parsed result.json survived, as jsonb on stage_invocations). The tables below
-- give every produced or edited artifact variant an immutable, content-addressed
-- revision row, and the manifest table records what went into a run so two runs
-- can be compared and reproduced.
--
-- Storage layout:
--   - Bytes live on the local FS in a canonical, worktree-independent location
--     (configured ArtifactRoot, default <repo>/.agentum/artifacts). The path is
--     derived from the content hash so identical content is stored once.
--   - The artifact_revisions row is the durable index; the FS blob is the body.
--   - Revisions chain via prev_revision_id: editing an artifact creates a new
--     revision that points back at the one it replaces. A revision already
--     referenced by a stage_invocation is never modified in place — the
--     invocation's link is immutable, so a later edit cannot retroactively
--     change what the agent saw.

CREATE TABLE artifact_revisions (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             uuid NOT NULL,
    user_id               uuid NOT NULL,
    task_id               uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,

    -- Identity within the task. `name` is the in-tree path or logical handle
    -- (e.g. "specs/auth.md", "result.json"); `kind` is the free-form category
    -- carried over from result.json artifacts (spec | code | adr | result_json …).
    name                  text NOT NULL,
    kind                  text NOT NULL DEFAULT 'file',

    -- Content addressing. sha256 hex of the stored bytes; the FS blob path is
    -- derived from this. Two revisions with the same hash share one blob.
    content_hash          text NOT NULL,
    content_size          bigint NOT NULL DEFAULT 0,

    -- Revision chain. action_type ∈ {create, edit}; prev_revision_id is NULL
    -- for the first revision of a (task_id, name) and points at the prior
    -- revision otherwise. Edits never overwrite; they chain.
    action_type           text NOT NULL,                       -- create | edit
    prev_revision_id      uuid REFERENCES artifact_revisions(id),

    -- Originating invocation, if any. NULL for human-authored edits via the API
    -- (the user_id column records who).
    source_invocation_id  uuid REFERENCES stage_invocations(id),

    -- Optional execution coordinate (delivery step / execution unit / phase).
    -- All three nullable: a plain single-unit run never fills them, and their
    -- absence MUST NOT change behavior. Epic 8 (multi-step delivery) populates
    -- them; until then they ride along as inert provenance.
    delivery_step         text,
    execution_unit        text,
    phase                 text,

    -- Actor: the user or system that produced this revision. Stored
    -- independently of user_id so a system-initiated edit (e.g. a fix-loop) is
    -- distinguishable from a human edit even when both are authorized by the
    -- same tenant.
    actor                 text NOT NULL,                       -- human | agent | system

    created_at            timestamptz NOT NULL DEFAULT now(),

    -- One current revision per (task, name). The "current" pointer is what
    -- resume / next-stage reads sync into the worktree. UNIQUE on the task +
    -- name + a sentinel "current" lets us use a partial index: at most one
    -- current row per name per task. We model currency as a separate column
    -- rather than "latest by created_at" so a human can pin a specific older
    -- revision as the one a resume syncs, and the FSM-visible pointer is
    -- explicit.
    is_current            boolean NOT NULL DEFAULT true
);

-- The current revision lookup must stay cheap and unique per (task, name).
CREATE UNIQUE INDEX idx_artifact_rev_current
    ON artifact_revisions(task_id, name) WHERE is_current;

-- Listing a task's revisions and jumping to a specific one.
CREATE INDEX idx_artifact_rev_task_name
    ON artifact_revisions(task_id, name, created_at DESC);
CREATE INDEX idx_artifact_rev_invocation
    ON artifact_revisions(source_invocation_id) WHERE source_invocation_id IS NOT NULL;
CREATE INDEX idx_artifact_rev_tenant ON artifact_revisions(tenant_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS artifact_revisions;

-- +goose StatementEnd
