-- +goose Up
-- +goose StatementBegin

-- 1. Repository identity is separated from the local path.
--
-- repo_path stops being a key: it changes on every move of the directory,
-- while identity must not. repo_identity is a fingerprint of the repository
-- itself, computed at the registration boundary (internal/repoid); the prefix
-- names the rule that derived it, so a future rule is a new prefix, not a
-- type migration. A directory that moved and was registered again resolves to
-- the same identity, therefore the same project row: run history, memory,
-- manifests and approvals stay with the repository.
ALTER TABLE projects ADD COLUMN repo_identity text NOT NULL;

ALTER TABLE projects DROP CONSTRAINT projects_tenant_id_repo_path_key;
CREATE UNIQUE INDEX idx_projects_identity ON projects(tenant_id, repo_identity);
-- Not unique: two different repositories that lived at the same path at
-- different times is a legitimate state, and nothing may forbid it. The index
-- serves resolving the previous path at re-registration.
CREATE INDEX idx_projects_repo_path ON projects(tenant_id, repo_path);

-- 2. A run pins the working copy it executes in.
--
-- The path stopped being the project's key, so it can now change under a run
-- that is already in flight: re-registering the same repository from another
-- clone would rewrite repo_path, and the continuation would create a new
-- worktree in the foreign copy off the pinned base — silently, with the whole
-- commit line lost. The working copy is therefore pinned on the run once,
-- exactly as base_commit already is. The empty string means "not pinned yet",
-- not "no value".
ALTER TABLE tasks ADD COLUMN checkout_path text NOT NULL DEFAULT '';

-- 3. tenant_id is brought to one type across all tables.
ALTER TABLE task_approvals ALTER COLUMN tenant_id TYPE uuid USING tenant_id::uuid;

-- 4. A decision gains a person, and actor becomes a vocabulary.
--
-- user_id answers "on whose behalf", actor "who acted"; the two coincide only
-- when a human acted. Same shape as artifact_revisions.
ALTER TABLE task_approvals ADD COLUMN user_id uuid NOT NULL;
ALTER TABLE task_approvals
    ADD CONSTRAINT task_approvals_actor_kind CHECK (actor IN ('human', 'agent', 'system'));

-- 5. An event names what produced it.
--
-- user_id stays "on whose behalf" and does not become nullable: the column's
-- meaning does not change, the new fact is a new column. There is no default
-- on purpose — a writer must name its actor, not get 'system' by silence.
ALTER TABLE events ADD COLUMN actor text NOT NULL;
ALTER TABLE events
    ADD CONSTRAINT events_actor_kind CHECK (actor IN ('human', 'agent', 'system'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE events DROP CONSTRAINT events_actor_kind;
ALTER TABLE events DROP COLUMN actor;

ALTER TABLE task_approvals DROP CONSTRAINT task_approvals_actor_kind;
ALTER TABLE task_approvals DROP COLUMN user_id;
ALTER TABLE task_approvals ALTER COLUMN tenant_id TYPE text USING tenant_id::text;

ALTER TABLE tasks DROP COLUMN checkout_path;

DROP INDEX idx_projects_repo_path;
DROP INDEX idx_projects_identity;
ALTER TABLE projects ADD CONSTRAINT projects_tenant_id_repo_path_key UNIQUE (tenant_id, repo_path);
ALTER TABLE projects DROP COLUMN repo_identity;

-- +goose StatementEnd
