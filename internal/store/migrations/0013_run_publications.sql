-- +goose Up
-- +goose StatementBegin

-- One publication per run: the delivery of a finished run to its Git
-- provider — the pushed branch and the draft pull request. Publication is an
-- outcome of delivery, not a state of the run: the run's FSM is untouched by
-- it, and every fact about the attempt lives here.
--
-- UNIQUE (run_id) is what makes "one run, at most one pull request" a
-- property of the schema rather than a check in a handler — the same device
-- that makes a repeated approval a no-op. The CHECK on state closes the
-- state vocabulary against typos, the same way run_approvals.actor_kind is
-- closed.
--
-- The target columns (provider, host, owner, repository, base branch) are
-- empty until the first attempt derives them from the checkout's remote; the
-- derived target is then frozen — a later move of the remote does not change
-- where a published delivery points. The table carries no credential and no
-- URL a credential could be recovered from: the token lives in the process
-- environment and reaches only the publisher.
--
-- ON DELETE CASCADE: the publication does not outlive its run.

CREATE TABLE run_publications (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          uuid NOT NULL,
    user_id            uuid NOT NULL,
    run_id             uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,

    provider           text NOT NULL,
    -- The target columns default to the empty string: the destination is
    -- derived at the first attempt and then frozen, so a row created at the
    -- gate carries no target yet and the NOT NULL vocabulary stays intact.
    target_host        text NOT NULL DEFAULT '',
    target_owner       text NOT NULL DEFAULT '',
    target_repository  text NOT NULL DEFAULT '',
    base_branch        text NOT NULL DEFAULT '',
    remote_branch      text NOT NULL,
    published_commit   text NOT NULL,

    state              text NOT NULL,
    pr_number          integer,
    pr_url             text,
    pr_state           text,

    branch_pushed_at   timestamptz,
    published_at       timestamptz,
    attempts           integer NOT NULL DEFAULT 0,
    last_error_code    text,
    last_error_message text,

    lease_owner        text,
    lease_expires_at   timestamptz,

    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT run_publications_state CHECK (
        state IN ('pending', 'publishing', 'published', 'failed', 'blocked')
    ),
    UNIQUE (run_id)
);

CREATE INDEX idx_run_publications_tenant_state ON run_publications(tenant_id, state);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_run_publications_tenant_state;
DROP TABLE IF EXISTS run_publications;

-- +goose StatementEnd
