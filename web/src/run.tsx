import React, { useCallback, useEffect, useMemo, useState } from "react";
import ReactMarkdown from "react-markdown";
import {
  ApiError,
  artifactContent,
  artifacts,
  invocations,
  project,
  run,
  startRun,
  type Artifact,
  type Invocation,
  type Project,
  type Run,
} from "./api";
import {
  Badge,
  Copy,
  Link,
  Loading,
  PageHeading,
  Problem,
  Reserved,
  Shell,
} from "./common";
import type { Navigate } from "./main";
import {
  bytes,
  duration,
  exact,
  relative,
  short,
  stateTone,
  terminalStates,
  waitingStates,
} from "./util";
const reasons: Record<string, [string, string]> = {
  gate: [
    "Waiting for approval",
    "The stage finished and its gate stops for a human decision.",
  ],
  plan_not_approved: [
    "Plan not approved",
    "A source-writing stage was about to start without an approved plan. The run waits at the plan gate.",
  ],
  plan_revision_drift: [
    "Plan changed after approval",
    "The plan's current revision differs from the one that was approved.",
  ],
  open_questions: [
    "The agent has questions",
    "The stage is blocked until the questions are answered.",
  ],
  parse_error: [
    "Stage result unreadable",
    "The agent finished without a valid result.json. Continuing re-runs the stage in the same session.",
  ],
  adapter_error: [
    "Agent runtime error",
    "The agent adapter reported an error: a crash, a timeout, or no output from the model.",
  ],
  artifact_rejected: [
    "Artifact refused",
    "A declared artifact escaped the worktree, could not be resolved, or contained a secret.",
  ],
  fix_budget_exhausted: [
    "Fix budget exhausted",
    "Review still requests changes and all fix cycles allowed by the pack are spent. Nothing was removed: branch, checkpoints and artifacts are kept.",
  ],
  verdict_unreadable: [
    "Review verdict unreadable",
    "The reviewer produced no parseable verdict.json, so the next stage cannot be chosen.",
  ],
  worktree_uncommitted_changes: [
    "Uncommitted changes in the worktree",
    "The resumed worktree holds uncommitted files. Someone must choose: resume over them, keep them as a checkpoint, or discard them.",
  ],
  base_ref_unresolvable: [
    "Base ref not found",
    "base_ref does not resolve to a commit in the run's working copy.",
  ],
  project_pack_drift: [
    "Project pack has uncommitted changes",
    "Files under .agentum/packs/ differ from HEAD. Commit or revert them, then continue.",
  ],
  project_pack_status_unreadable: [
    "Pack status unreadable",
    "git status failed for the pack directory in the working copy.",
  ],
  base_target_unverifiable: [
    "Base cannot be checked against the target",
    "The publication target branch could not be compared with the run's base.",
  ],
  base_not_on_target: [
    "Base is not on the target branch",
    "The base carries commits that the publication target branch does not have.",
  ],
  checkout_unavailable: [
    "Working copy unavailable",
    "The run's working copy is missing or now holds a different repository. Restore the directory or re-register the project, then continue.",
  ],
  interrupted: [
    "Interrupted",
    "The worker stopped in the middle of a stage (restart or crash). Continuing resumes the captured session.",
  ],
  continue_payload_unreadable: [
    "Continue request unreadable",
    "The continue job's payload could not be decoded, so the agent was not invoked.",
  ],
  resume_session_missing: [
    "No session to resume",
    "Continue carried text, but the latest invocation has no captured session.",
  ],
  worktree_branch_unconfirmed: [
    "Branch moved after discard",
    "The run branch tip differs from the HEAD confirmed when the worktree was discarded.",
  ],
};
function StopPanel({
  item,
  history,
  revisions,
  startError,
  openRevision,
  now,
}: {
  item: Run;
  history: Invocation[];
  revisions: Artifact[];
  startError: string;
  openRevision: (revision: Artifact) => void;
  now: number;
}) {
  const latest = history.at(-1),
    state = item.state,
    reason = item.stop_reason,
    reasonCopy = reasons[reason] || [
      "Unrecognised stop reason",
      "This version of the UI has no description for this code. Search the run log for it.",
    ];
  let eyebrow = "",
    title = "",
    body = "",
    action = "",
    artifactName = "",
    kept: string[] = [];
  switch (state) {
    case "created":
      eyebrow = startError ? "Not started · start failed" : "Not started";
      title = startError
        ? "The run was created, but starting it failed."
        : "The run is created and has not started.";
      body = startError
        ? startError
        : `Starting resolves base_ref to a commit, creates the branch ${item.branch || "agentum/" + short(item.id)} and runs the first stage, plan.`;
      break;
    case "running":
      eyebrow = "Working · no action needed";
      title = latest
        ? `Invocation #${latest.sequence} · ${latest.stage} · cycle ${latest.cycle}`
        : "The run is starting.";
      body = latest
        ? "The agent is working. This page updates every 3 seconds."
        : "The first invocation will appear here when it starts.";
      break;
    case "paused_gate":
      eyebrow = "Waiting for you";
      title =
        reason && reason !== "gate"
          ? reasonCopy[0]
          : "The plan is ready for approval.";
      body =
        reason && reason !== "gate"
          ? reasonCopy[1]
          : "No source code changes until the plan is approved.";
      action = "advance";
      artifactName = "plan/plan.md";
      break;
    case "paused_open_questions":
      eyebrow = "Waiting for you";
      title = `The agent asked ${(item.open_questions || []).length} questions in ${item.current_stage || "plan"}.`;
      body = "The stage is blocked until they are answered.";
      action = "continue {text}";
      artifactName = (item.current_stage || "plan") + "/result.json";
      break;
    case "paused_user_stop":
      eyebrow = "Stopped · waiting for you";
      title = reasonCopy[0];
      body = reasonCopy[1];
      action = "continue";
      break;
    case "awaiting_final_review":
      eyebrow = "Waiting for you · final review";
      title = "The work is ready for final review.";
      body = `Result commit ${short(item.result_commit, 7)} on ${item.branch}.`;
      action = "approve";
      artifactName = "review/verdict.json";
      break;
    case "done":
      eyebrow = "Done";
      title = "Accepted at final review.";
      body = `Result commit ${short(item.result_commit, 7)}. The worktree was removed; the branch is kept.`;
      kept = [
        `branch ${item.branch}`,
        `result_commit ${short(item.result_commit, 7)}`,
        `${revisions.length} artifacts`,
      ];
      break;
    case "failed":
      eyebrow = "Failed · not resumable";
      title = `The run failed${item.current_stage ? " in " + item.current_stage : ""}.`;
      body =
        "Your work is saved: nothing was removed. A new run can start from any of the kept commits.";
      kept = [`branch ${item.branch}`];
      break;
    case "cancelled":
      const rejectedAtPlan = item.cancel_reason === "rejected_at_plan";
      const rejectedAtFinal = item.cancel_reason === "rejected_at_final_review";
      const rejected =
        rejectedAtPlan || rejectedAtFinal || item.cancel_reason === "rejected";
      eyebrow =
        rejectedAtPlan
          ? "Cancelled · rejected at plan gate"
          : rejectedAtFinal
            ? "Cancelled · rejected at final review"
            : rejected
              ? "Cancelled · rejected"
              : "Cancelled";
      title =
        rejectedAtPlan
          ? "The plan was rejected."
          : rejectedAtFinal
            ? "The result was rejected."
            : rejected
              ? "The run was rejected."
              : `The run was cancelled${item.current_stage ? " during " + item.current_stage : ""}.`;
      body = "The worktree was removed; the branch is kept for reference.";
      kept = [`branch ${item.branch}`];
      if (item.result_commit)
        kept.push(`result_commit ${short(item.result_commit, 7)}`);
      break;
  }
  const match =
    revisions.find(
      (revision) => revision.name === artifactName && revision.is_current,
    ) || revisions.find((revision) => revision.name === artifactName);
  const actionPath = latest
    ? `POST /api/v1/runs/${item.id}/invocations/${latest.id}/${action}`
    : `POST /api/v1/runs/${item.id}`;
  if (waitingStates.has(state)) {
    const elapsedMinutes = Math.max(
      0,
      Math.floor((now - Date.parse(item.updated_at)) / 60000),
    );
    eyebrow += ` · ${elapsedMinutes}m`;
  }
  return (
    <section className={"stop-panel " + stateTone(state)}>
      <div>
        <div className="eyebrow">
          {eyebrow}
          {reason && waitingStates.has(state) && <code>{reason}</code>}
        </div>
        <h2>{title}</h2>
        <p>{body}</p>
        {state === "paused_open_questions" && item.open_questions && (
          <ol className="questions">
            {item.open_questions.map((question, index) => (
              <li key={index}>{question}</li>
            ))}
          </ol>
        )}
        {match && (
          <button className="artifact-link" onClick={() => openRevision(match)}>
            {state === "paused_gate"
              ? "Awaiting decision:"
              : state === "awaiting_final_review"
                ? "Verdict:"
                : "Source:"}{" "}
            <code>{match.name}</code>
          </button>
        )}
        {state === "failed" && item.error && (
          <pre className="failure-detail">{item.error}</pre>
        )}
        {kept.length > 0 && (
          <ul className="kept">
            {kept.map((value) => (
              <li key={value}>✓ {value}</li>
            ))}
          </ul>
        )}
      </div>
      <div className="stop-side">
        {state === "running" && latest ? (
          <div className="live-time">
            <strong>
              {duration(latest.started_at, new Date(now).toISOString())}
            </strong>
            <span>
              since{" "}
              {new Date(latest.started_at).toLocaleTimeString("en", {
                hour12: false,
              })}
            </span>
          </div>
        ) : (
          action && (
            <div className="api-hint">
              <span>Action is available via API for now</span>
              <code>{actionPath}</code>
            </div>
          )
        )}
      </div>
    </section>
  );
}
function Invocations({ items, state }: { items: Invocation[]; state: string }) {
  const latest = items.at(-1),
    fixCycles = Math.max(0, ...items.map((item) => item.cycle));
  return (
    <section className="data-section">
      <h2 className="section-title">
        Invocations{" "}
        <span>
          {items.length
            ? `${items.length} ${items.length === 1 ? "attempt" : "attempts"} · ${fixCycles} fix cycles`
            : ""}
        </span>
      </h2>
      {!items.length ? (
        <div className="empty-section">
          No invocations yet. The first one starts with the <code>plan</code>{" "}
          stage.
        </div>
      ) : (
        <div className="table invocation-table">
          <div className="table-head invocation-grid">
            <span>seq</span>
            <span>stage</span>
            <span>cycle</span>
            <span>started</span>
            <span>finished</span>
            <span>duration</span>
            <span>stop reason</span>
          </div>
          {items.map((item) => (
            <div
              key={item.id}
              className={
                "table-row invocation-grid " +
                (item.id === latest?.id && waitingStates.has(state)
                  ? "waiting-row"
                  : "")
              }
            >
              <span>
                <i
                  className={
                    "dot " +
                    (!item.finished_at
                      ? "blue"
                      : item.id === latest?.id && state === "failed"
                        ? "red"
                        : item.id === latest?.id && waitingStates.has(state)
                          ? "amber"
                          : "gray")
                  }
                />
                {item.sequence}
              </span>
              <span className="truncate">
                {item.stage}
                {item.resume_of && (
                  <small className="resume-chip" title={item.resume_of}>
                    resume of #
                    {items.find((previous) => previous.id === item.resume_of)
                      ?.sequence || "?"}
                  </small>
                )}
              </span>
              <code>{item.cycle}</code>
              <time title={exact(item.started_at)}>
                {relative(item.started_at)}
              </time>
              <time title={exact(item.finished_at || "")}>
                {item.finished_at ? relative(item.finished_at) : "running"}
              </time>
              <code>{duration(item.started_at, item.finished_at)}</code>
              <code className="truncate" title={item.stop_reason || ""}>
                {item.stop_reason || "—"}
              </code>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}
function ArtifactViewer({
  item,
  runID,
  close,
}: {
  item: Artifact;
  runID: string;
  close: () => void;
}) {
  const [text, setText] = useState<string | null>(null),
    [error, setError] = useState<unknown>(null);
  useEffect(() => {
    setText(null);
    setError(null);
    let active = true;
    artifactContent(runID, item.id)
      .then((content) => {
        if (active) setText(content);
      })
      .catch((caught) => {
        if (active) setError(caught);
      });
    return () => {
      active = false;
    };
  }, [runID, item.id]);
  const kind =
    item.name.endsWith(".md") ||
    ["md", "markdown", "spec", "adr"].includes(item.kind)
      ? "md"
      : item.name.endsWith(".json") ||
          ["json", "result_json"].includes(item.kind)
        ? "json"
        : item.name.endsWith(".diff") || item.name.endsWith(".patch")
          ? "diff"
          : "text";
  return (
    <div className="viewer">
      <div className="viewer-head">
        <div>
          <strong>{item.name}</strong>
          <span className="mono">
            {item.actor} · {item.content_size} bytes · sha256{" "}
            {short(item.content_hash, 7)}
          </span>
        </div>
        <button className="secondary" onClick={close}>
          Close
        </button>
      </div>
      {error ? (
        <Problem error={error} title="Artifact could not be loaded." />
      ) : text === null ? (
        <Loading label="Loading artifact…" />
      ) : kind === "md" ? (
        <div className="markdown">
          <ReactMarkdown skipHtml>{text}</ReactMarkdown>
        </div>
      ) : kind === "diff" ? (
        <pre className="diff">
          {text.split("\n").map((line, index) => (
            <span
              key={index}
              className={
                line.startsWith("+") && !line.startsWith("+++")
                  ? "added"
                  : line.startsWith("-") && !line.startsWith("---")
                    ? "removed"
                    : line.startsWith("@@")
                      ? "hunk"
                      : ""
              }
            >
              {line + "\n"}
            </span>
          ))}
        </pre>
      ) : (
        <pre className="artifact-text">{text}</pre>
      )}
    </div>
  );
}
function Artifacts({
  items,
  runID,
  open,
  setOpen,
}: {
  items: Artifact[];
  runID: string;
  open: Artifact | null;
  setOpen: (value: Artifact | null) => void;
}) {
  const ordered = useMemo(
    () =>
      [...items].sort((first, second) =>
        first.created_at.localeCompare(second.created_at),
      ),
    [items],
  );
  const revisions = new Map<string, number>();
  const revisionNumbers = new Map<string, number>();
  for (const item of ordered) {
    const next = (revisions.get(item.name) || 0) + 1;
    revisions.set(item.name, next);
    revisionNumbers.set(item.id, next);
  }
  return (
    <section className="data-section">
      <h2 className="section-title">
        Artifacts <span>{items.length} revisions</span>
      </h2>
      {!items.length ? (
        <div className="empty-section">
          No artifacts yet. Stages write them as they finish.
        </div>
      ) : (
        <div className="table artifact-table">
          <div className="table-head artifact-grid">
            <span>name</span>
            <span>kind</span>
            <span>rev</span>
            <span>actor</span>
            <span>created</span>
          </div>
          {ordered.map((item) => (
            <button
              key={item.id}
              className={
                "table-row artifact-grid " +
                (open?.id === item.id ? "selected" : "")
              }
              aria-pressed={open?.id === item.id}
              onClick={() => setOpen(open?.id === item.id ? null : item)}
            >
              <span className="truncate mono" title={item.name}>
                {item.name}
              </span>
              <span>{item.kind}</span>
              <code>{revisionNumbers.get(item.id)}</code>
              <span>{item.actor}</span>
              <time title={exact(item.created_at)}>
                {relative(item.created_at)}
              </time>
            </button>
          ))}
        </div>
      )}
      {open && (
        <ArtifactViewer
          key={open.id}
          item={open}
          runID={runID}
          close={() => setOpen(null)}
        />
      )}
    </section>
  );
}
function Facts({ item }: { item: Run }) {
  const facts: [string, string, boolean][] = [
    ["base_ref", item.base_ref, false],
    ["base_commit", item.base_commit || "set on start", !!item.base_commit],
    ["branch", item.branch, true],
    ...(item.result_commit
      ? [
          ["result_commit", item.result_commit, true] as [
            string,
            string,
            boolean,
          ],
        ]
      : []),
    ["pack", item.pipeline_pack, false],
    ["pack origin", item.pipeline_pack_origin || "set on start", false],
    ["run id", item.id, true],
  ];
  return (
    <section className="facts">
      <h2 className="section-title">Run</h2>
      {facts.map(([key, value, copy]) => (
        <div className="fact" key={key}>
          <span>{key}</span>
          <code title={value}>
            {value === "set on start"
              ? value
              : key === "base_ref"
                ? value
                : short(value, key === "branch" ? 20 : 8)}
          </code>
          {copy && <Copy value={value} label={"Copy " + key} />}
        </div>
      ))}
    </section>
  );
}
export function RunPage({
  runID,
  navigate,
}: {
  runID: string;
  navigate: Navigate;
}) {
  const [item, setItem] = useState<Run | null>(null),
    [info, setInfo] = useState<Project | null>(null),
    [history, setHistory] = useState<Invocation[]>([]),
    [revisions, setRevisions] = useState<Artifact[]>([]),
    [error, setError] = useState<unknown>(null),
    [stale, setStale] = useState<{ count: number; error: unknown } | null>(
      null,
    ),
    [lastGood, setLastGood] = useState(""),
    [open, setOpen] = useState<Artifact | null>(null),
    [viewerTouched, setViewerTouched] = useState(false),
    [expanded, setExpanded] = useState(false),
    [now, setNow] = useState(Date.now()),
    [startError, setStartError] = useState(
      () => sessionStorage.getItem("agentum.start_error." + runID) || "",
    ),
    [starting, setStarting] = useState(false);
  const load = useCallback(async () => {
    try {
      const data = await run(runID);
      const [attempts, artifactRows] = await Promise.all([
        invocations(runID),
        artifacts(runID),
      ]);
      setItem(data);
      setHistory(attempts);
      setRevisions(artifactRows);
      setLastGood(new Date().toISOString());
      setStale(null);
      setError(null);
      if (data.state !== "created") {
        setStartError("");
        sessionStorage.removeItem("agentum.start_error." + runID);
      }
      project(data.project_id)
        .then(setInfo)
        .catch(() => {});
    } catch (caught) {
      setStale((previous) =>
        item ? { count: (previous?.count || 0) + 1, error: caught } : null,
      );
      if (!item) setError(caught);
    }
  }, [runID, item]);
  useEffect(() => {
    setItem(null);
    setError(null);
    void load();
  }, [runID]);
  useEffect(() => {
    if (item && terminalStates.has(item.state)) return;
    const timer = setInterval(() => {
      void load();
    }, 3000);
    return () => clearInterval(timer);
  }, [runID, item?.state, load]);
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  useEffect(() => {
    if (open && !revisions.some((revision) => revision.id === open.id))
      setOpen(null);
  }, [revisions]);
  useEffect(() => {
    if (viewerTouched || open || !item) return;
    const desired =
      item.state === "paused_gate"
        ? "plan/plan.md"
        : item.state === "paused_open_questions"
          ? (item.current_stage || "plan") + "/result.json"
          : item.state === "awaiting_final_review"
            ? "review/notes.md"
            : item.state === "paused_user_stop"
              ? "review/notes.md"
              : "";
    if (desired) {
      const match =
        revisions.find(
          (revision) => revision.name === desired && revision.is_current,
        ) || revisions.find((revision) => revision.name === desired);
      if (match) setOpen(match);
    }
  }, [item?.state, revisions, viewerTouched, open]);
  const selectRevision = (revision: Artifact | null) => {
    setViewerTouched(true);
    setOpen(revision);
  };
  const start = async () => {
    if (starting) return;
    setStarting(true);
    try {
      await startRun(runID);
      setStartError("");
      sessionStorage.removeItem("agentum.start_error." + runID);
      void load();
    } catch (caught) {
      const message = caught instanceof Error ? caught.message : String(caught);
      setStartError(message);
      sessionStorage.setItem("agentum.start_error." + runID, message);
    } finally {
      setStarting(false);
    }
  };
  const poll =
    item && terminalStates.has(item.state)
      ? "Final state · polling stopped"
      : stale
        ? `Stale · last update ${relative(lastGood)}`
        : item
          ? "Updated just now · polling every 3s"
          : "Loading…";
  return (
    <Shell
      navigate={navigate}
      crumbs={[
        { label: "Projects", to: "/projects" },
        ...(item
          ? [
              {
                label: info?.name || "project",
                to: "/projects/" + item.project_id,
              },
            ]
          : []),
        { label: item ? short(item.id) : "run" },
      ]}
      poll={poll}
      offline={!!stale || (error instanceof ApiError && error.status === 0)}
    >
      {stale && item && (
        <div className="stale" role="status">
          <strong>
            Stale · {stale.count} failed {stale.count === 1 ? "poll" : "polls"}
          </strong>
          <span>
            Last good update {exact(lastGood)}.{" "}
            <code>
              {stale.error instanceof Error
                ? stale.error.message
                : String(stale.error)}
            </code>
          </span>
          <button className="secondary" onClick={() => void load()}>
            Retry now
          </button>
        </div>
      )}
      {!item && !error && (
        <div className="run-loading">
          <Loading label="Loading run…" />
          <div className="skeleton panel-skeleton" />
          <div className="run-columns">
            <div className="skeleton tall" />
            <div className="skeleton tall" />
          </div>
        </div>
      )}
      {!item && !!error && (
        <Problem
          error={error}
          title={
            error instanceof ApiError && error.status === 0
              ? `Agentum is not responding at ${location.host}.`
              : "This run could not be loaded."
          }
          onRetry={() => void load()}
          hint={"GET /api/v1/runs/" + runID}
        />
      )}{" "}
      {item && (
        <>
          <div className="run-heading">
            <div>
              <h1>{item.title}</h1>
              <div className="run-meta">
                <Badge state={item.state} />
                <span>
                  Stage <code>{item.current_stage || "—"}</code>
                </span>
                <span>
                  Created{" "}
                  <time title={exact(item.created_at)}>
                    {relative(item.created_at)}
                  </time>
                </span>
                <span>
                  Changed{" "}
                  <time title={exact(item.updated_at)}>
                    {relative(item.updated_at)}
                  </time>
                </span>
              </div>
            </div>
            <div className="run-heading-actions">
              {item.state === "created" && (
                <button
                  className="primary"
                  disabled={starting}
                  onClick={() => void start()}
                >
                  {starting ? "Starting…" : "Start run"}
                </button>
              )}
              <div className="header-api-hints">
                <span>Reserved · actions via API</span>
                {!terminalStates.has(item.state) && (
                  <code>POST /api/v1/runs/{item.id}/cancel</code>
                )}
                {(item.state === "awaiting_final_review" ||
                  item.state === "paused_gate") && (
                  <code>POST /api/v1/runs/{item.id}/reject</code>
                )}
              </div>
            </div>
          </div>
          <div className="route-placeholder">
            <span>Route</span>
            <Reserved>selected route and the reason it was chosen</Reserved>
          </div>
          <StopPanel
            item={item}
            history={history}
            revisions={revisions}
            startError={startError}
            openRevision={selectRevision}
            now={now}
          />
          <div className="tabs">
            <span className="active">Overview</span>
            <span className="disabled">
              Statistics <small>reserved</small>
            </span>
          </div>
          <div className="run-columns">
            <div className="run-main">
              <section className="data-section">
                <h2 className="section-title">
                  Request <span>{bytes(item.description)} bytes</span>
                </h2>
                <div className={"request-text " + (expanded ? "expanded" : "")}>
                  {item.description}
                </div>
                {item.description.length > 150 && (
                  <button
                    className="text-button expand"
                    aria-expanded={expanded}
                    onClick={() => setExpanded(!expanded)}
                  >
                    {expanded ? "Show less" : "Show full description"}
                  </button>
                )}
              </section>
              <Invocations items={history} state={item.state} />
              <Artifacts
                items={revisions}
                runID={runID}
                open={open}
                setOpen={selectRevision}
              />
            </div>
            <aside className="run-aside">
              <Facts item={item} />
              <Reserved>Result</Reserved>
            </aside>
          </div>
        </>
      )}
    </Shell>
  );
}
