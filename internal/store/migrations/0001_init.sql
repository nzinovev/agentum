-- +goose Up
-- +goose StatementBegin

-- store.Open creates the agentum schema before goose runs, so all objects
-- (including goose's own version table) land here. search_path is pinned to
-- agentum by the DSN; the SET below is belt-and-suspenders for the CLI path.
CREATE SCHEMA IF NOT EXISTS agentum;
SET search_path TO agentum;

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Tasks: the top-level orchestration unit. One task = one run of a pipeline
-- against a target project. Lifecycle is the explicit FSM in internal/engine.
-- Every row carries the tenant/user seam.
CREATE TABLE tasks (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL,
    user_id       uuid NOT NULL,
    project_id    uuid NOT NULL,
    pipeline_pack text NOT NULL,                       -- e.g. "java-spring@1"
    title         text NOT NULL,
    input         jsonb NOT NULL DEFAULT '{}',
    state         text NOT NULL DEFAULT 'created',     -- engine.TaskState
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Stage invocations: each agent run within a task. session_id is what makes
-- user-stop / cancel non-destructive; resume_of chains resumes. stop_reason +
-- pending_edits feed the routing-block edit-notice on resume.
CREATE TABLE stage_invocations (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL,
    user_id       uuid NOT NULL,
    task_id       uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    stage         text NOT NULL,                       -- e.g. "spec" | "implement" | "review"
    sequence      integer NOT NULL,                    -- order within the task
    session_id    text,                                -- from agent JSON stream; nullable
    resume_of     uuid REFERENCES stage_invocations(id), -- self-ref for resumes
    stop_reason   text,                                -- open_questions | gate | user_stop; null if not stopped
    pending_edits jsonb NOT NULL DEFAULT '[]',         -- artifacts edited during the pause
    result        jsonb,                               -- parsed result.json
    started_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz
);

-- Project memory. Only project scope is wired at MVP; user and org scopes are
-- present but inert. Rows are inserted only once a task reaches final approval
-- — staged entries commit at done.
CREATE TABLE memory_entries (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid NOT NULL,
    user_id        uuid NOT NULL,
    project_id     uuid NOT NULL,
    scope          text NOT NULL DEFAULT 'project',    -- project | user | org (only project wired)
    kind           text NOT NULL,                      -- decision | convention | spec_ref | fix | note
    title          text NOT NULL,
    body           text NOT NULL,                      -- distilled decision; kept short (NOT a whole artifact)
    keywords       text[] NOT NULL DEFAULT '{}',
    source_task_id uuid REFERENCES tasks(id),
    source_stage   text,
    status         text NOT NULL DEFAULT 'active',     -- active | superseded (nothing sets superseded at MVP)
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Durable event log: the single stream SSE replays from. One schema serves the
-- audit trail and UI reconnect. id is monotonic, so Last-Event-ID is a simple
-- "> last_id" tail.
CREATE TABLE events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id  uuid NOT NULL,
    user_id    uuid NOT NULL,
    task_id    uuid REFERENCES tasks(id) ON DELETE CASCADE,
    type       text NOT NULL,                          -- e.g. task.state_changed | stage.stopped | memory.committed
    payload    jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_tasks_tenant_project  ON tasks(tenant_id, project_id);
CREATE INDEX idx_tasks_state           ON tasks(tenant_id, state);
CREATE INDEX idx_stages_task           ON stage_invocations(task_id, sequence);
CREATE INDEX idx_memory_project_recent ON memory_entries(project_id, scope, created_at DESC);
CREATE INDEX idx_events_task           ON events(task_id, id);
CREATE INDEX idx_events_tenant_created ON events(tenant_id, created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS memory_entries;
DROP TABLE IF EXISTS stage_invocations;
DROP TABLE IF EXISTS tasks;

-- +goose StatementEnd
