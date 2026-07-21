-- +goose Up
-- +goose StatementBegin

-- Projects: one repo = one project (04 §2.6). repo_path is unique per tenant
-- (idempotent registration — registering the same repo returns the existing
-- row). related_projects is an inert seam for the deferred cross-project /
-- sibling-folder design (backlog "Deferred"): stored now, enforced later in
-- Epic 6 as a path-scoped fs.read capability derived from this set, never
-- auto-discovered (the user-configured relation is the security boundary).
CREATE TABLE projects (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         uuid NOT NULL,
    user_id           uuid NOT NULL,
    repo_path         text NOT NULL,
    name              text NOT NULL,
    related_projects  uuid[] NOT NULL DEFAULT '{}',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, repo_path)
);

CREATE INDEX idx_projects_tenant ON projects(tenant_id);

-- tasks.project_id was a dangling NOT NULL uuid in 0001 (the projects table did
-- not exist yet). Make it a real FK. ON DELETE RESTRICT protects task history:
-- a project with tasks cannot be removed. Pre-existing task rows that reference
-- no project (only possible on a dev DB carrying stale data from before this
-- migration) must be cleared first — this is a one-time reconciliation for the
-- greenfield pre-production schema, never re-run.
ALTER TABLE tasks
    ADD CONSTRAINT tasks_project_fk
    FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE RESTRICT;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_project_fk;
DROP TABLE IF EXISTS projects;

-- +goose StatementEnd
