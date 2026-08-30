-- +goose Up
-- +goose StatementBegin

-- The task request becomes a typed contract. `description` is the
-- requested behaviour delivered to the agent; `overrides` is the
-- orchestrator-facing run configuration. The untyped `input` container is
-- removed rather than deprecated: while it exists, the next ill-fitting field
-- lands in it and the silent-typo failure mode returns.
ALTER TABLE tasks
    ADD COLUMN description text  NOT NULL DEFAULT '',
    ADD COLUMN overrides   jsonb NOT NULL DEFAULT '{}';

-- Backfill by convention: `input.description` is what every existing row uses
-- (nothing read it, but everything wrote it), and `input.checks` is the only
-- key the runner ever consumed. Any other key was already inert.
--
-- The check request is rebuilt field by field rather than copied wholesale.
-- The old reader was a lenient json.Unmarshal that ignored unknown keys and
-- non-conforming shapes; the new one rejects them. Copying
-- `input->'checks'` verbatim would migrate a row that ran fine for months into
-- one that fails its next run — that loudness belongs on new writes at
-- the API boundary, not retroactively on rows nobody can go back and fix.
-- So: keep only `required`/`optional` when they are arrays, keep only their
-- string elements, and fall back to `{}`. A legacy check request too malformed
-- to translate is dropped, which is what the lenient reader effectively did
-- with it anyway; the description, which is what matters, is always preserved.
WITH migrated AS (
    SELECT task.id,
           coalesce(task.input->>'description', '') AS description,
           jsonb_strip_nulls(jsonb_build_object(
               'required', (
                   SELECT jsonb_agg(element)
                   FROM jsonb_array_elements(
                       CASE WHEN jsonb_typeof(task.input->'checks'->'required') = 'array'
                            THEN task.input->'checks'->'required' ELSE '[]'::jsonb END
                   ) AS names(element)
                   WHERE jsonb_typeof(element) = 'string'
               ),
               'optional', (
                   SELECT jsonb_agg(element)
                   FROM jsonb_array_elements(
                       CASE WHEN jsonb_typeof(task.input->'checks'->'optional') = 'array'
                            THEN task.input->'checks'->'optional' ELSE '[]'::jsonb END
                   ) AS names(element)
                   WHERE jsonb_typeof(element) = 'string'
               )
           )) AS checks
    FROM tasks AS task
)
UPDATE tasks SET
    description = migrated.description,
    overrides   = CASE WHEN migrated.checks = '{}'::jsonb
                       THEN '{}'::jsonb
                       ELSE jsonb_build_object('checks', migrated.checks) END
FROM migrated
WHERE tasks.id = migrated.id;

-- The default exists only to make the ADD COLUMN valid against existing rows.
-- New rows get a non-empty description from the API, which rejects an empty one.
ALTER TABLE tasks
    ALTER COLUMN description DROP DEFAULT,
    DROP COLUMN input;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tasks ADD COLUMN input jsonb NOT NULL DEFAULT '{}';
UPDATE tasks SET input = jsonb_strip_nulls(
    jsonb_build_object('description', nullif(description, ''))
    || CASE WHEN overrides ? 'checks'
            THEN jsonb_build_object('checks', overrides->'checks')
            ELSE '{}'::jsonb END
);
ALTER TABLE tasks DROP COLUMN overrides, DROP COLUMN description;
-- +goose StatementEnd
