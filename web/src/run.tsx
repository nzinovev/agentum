import React, { useCallback, useEffect, useMemo, useState } from "react";
import ReactMarkdown from "react-markdown";
import {
  ApiError,
  artifactContent,
  artifacts,
  invocations,
  finalReview,
  publication,
  savePlan,
  project,
  run,
  startRun,
  type Artifact,
  type Invocation,
  type FinalReview,
  type Publication,
  type Project,
  type Run,
} from "./api";
import {
  Badge,
  Copy,
  Loading,
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
  terminalStates,
  waitingStates,
} from "./util";
import { RunActionPanel } from "./run-actions";

function resourceStatus(state?: string): string {
  return ({ present: "Kept", removing: "Removing…", removed: "Removed", not_created: "Not created" } as Record<string, string>)[state || ""] || "—";
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
  editing,
  startEditing,
  latestInvocation,
  canEdit,
  nextRevision,
  stopEditing,
  saved,
  refresh,
}: {
  item: Artifact;
  runID: string;
  close: () => void;
  editing: boolean;
  startEditing: () => void;
  latestInvocation?: Invocation;
  canEdit: boolean;
  nextRevision: number;
  stopEditing: () => void;
  saved: (revision: Artifact) => void;
  refresh: () => Promise<void>;
}) {
  const [text, setText] = useState<string | null>(null),
    [error, setError] = useState<unknown>(null),
    [draft, setDraft] = useState(""),
    [saveError, setSaveError] = useState<unknown>(null),
    [saving, setSaving] = useState(false);
  useEffect(() => {
    setText(null);
    setError(null);
    let active = true;
    artifactContent(runID, item.id)
      .then((content) => {
        if (active) { setText(content); setDraft(content); }
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
  const save = async () => {
    if (!latestInvocation || text === null || draft === text || saving) return;
    setSaving(true); setSaveError(null);
    try {
      const revision = await savePlan(runID, latestInvocation.id, draft, item.id);
      stopEditing(); saved(revision);
    } catch (caught) { setSaveError(caught); if (caught instanceof ApiError && (caught.status === 409 || caught.status === 428)) await refresh(); }
    finally { setSaving(false); }
  };
  return (
    <div className={"viewer " + (editing ? "plan-editing" : "")} id="artifact-viewer">
      <div className="viewer-head">
        <div>
          <strong>{item.name}</strong>
          <span className="mono">{editing ? `editing · base rev ${item.id}` : `${item.actor} · ${item.content_size} bytes · sha256 ${short(item.content_hash, 7)}`}</span>
        </div>
        {item.name === "plan/plan.md" && canEdit && !editing && latestInvocation && <button className="secondary" onClick={startEditing}>Edit</button>}
        <button className="secondary" onClick={() => { stopEditing(); close(); }}>
          Close
        </button>
      </div>
      {error ? (
        <Problem error={error} title="Artifact could not be loaded." />
      ) : text === null ? (
        <Loading label="Loading artifact…" />
      ) : editing ? <>
        {saveError && (saveError instanceof ApiError && (saveError.status === 409 || saveError.status === 428) ?
          <div className="run-notice human" role="alert"><strong>Plan changed. Review the current revision before trying again. Your edited text is kept and was not saved.</strong><code>{saveError.status} {saveError.code} · {saveError.message}</code></div> :
          <Problem error={saveError} title="Plan was not saved. Your edited text is kept." />)}
        <textarea className="plan-editor" value={draft} onChange={(event) => setDraft(event.target.value)} aria-invalid={!!saveError} />
        <div className="run-form-footer"><button className="primary" disabled={saving || !canEdit || draft === text || !draft.trim()} onClick={() => void save()}>{saving ? "Saving…" : `Save as rev ${nextRevision}`}</button><button className="secondary" disabled={saving} onClick={stopEditing}>Discard changes</button><span>Saving creates a new revision. The run stays at the gate until you approve it.</span></div>
      </> : kind === "md" ? (
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
  editing,
  startEditing,
  latestInvocation,
  canEdit,
  stopEditing,
  saved,
  refresh,
}: {
  items: Artifact[];
  runID: string;
  open: Artifact | null;
  setOpen: (value: Artifact | null) => void;
  editing: boolean;
  startEditing: () => void;
  latestInvocation?: Invocation;
  canEdit: boolean;
  stopEditing: () => void;
  saved: (revision: Artifact) => void;
  refresh: () => Promise<void>;
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
          editing={editing && open.name === "plan/plan.md"}
          startEditing={startEditing}
          latestInvocation={latestInvocation}
          canEdit={canEdit}
          nextRevision={(revisions.get("plan/plan.md") || 0) + 1}
          stopEditing={stopEditing}
          saved={saved}
          refresh={refresh}
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
  const [review, setReview] = useState<FinalReview | null>(null);
  const [delivery, setDelivery] = useState<Publication | null>(null);
  const [stat, setStat] = useState("");
  const [editing, setEditing] = useState(false);
  const [savedRevisionID, setSavedRevisionID] = useState("");
  const [cancelSignal, setCancelSignal] = useState(0);
  const [removalPending, setRemovalPending] = useState(false);
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
      if (["awaiting_final_review", "done", "cancelled"].includes(data.state)) {
        finalReview(runID).then((result) => {
          setReview(result);
          if (result.diff?.stat_revision_id) artifactContent(runID, result.diff.stat_revision_id).then(setStat).catch(() => setStat(""));
        }).catch(() => setReview(null));
      }
      if (["awaiting_final_review", "done"].includes(data.state)) publication(runID).then(setDelivery).catch(() => setDelivery({ state: "unavailable" }));
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
    const sealing = item && ["done", "cancelled"].includes(item.state) && item.branch_state === "present" && !item.result_commit;
    if (item && terminalStates.has(item.state) && !removalPending && !sealing && !["pending", "publishing"].includes(delivery?.state || "")) return;
    const timer = setInterval(() => {
      void load();
    }, 3000);
    return () => clearInterval(timer);
  }, [runID, item?.state, load, removalPending, delivery?.state]);
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
  const openAndScroll = (revision: Artifact) => {
    selectRevision(revision);
    setTimeout(() => document.getElementById("artifact-viewer")?.scrollIntoView({ behavior: "smooth", block: "start" }), 0);
  };
  const editPlan = () => {
    const plan = revisions.find((revision) => revision.name === "plan/plan.md" && revision.is_current);
    if (!plan) return;
    openAndScroll(plan); setEditing(true);
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
    item && terminalStates.has(item.state) && !removalPending && !(["done", "cancelled"].includes(item.state) && item.branch_state === "present" && !item.result_commit) && !["pending", "publishing"].includes(delivery?.state || "")
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
                  disabled={starting || !!stale}
                  onClick={() => void start()}
                >
                  {starting ? "Starting…" : "Start run"}
                </button>
              )}
              {!terminalStates.has(item.state) && <button className="secondary" disabled={!!stale} onClick={() => setCancelSignal((signal) => signal + 1)}>Cancel run</button>}
            </div>
          </div>
          <div className="route-placeholder">
            <span>Route</span>
            <Reserved>selected route and the reason it was chosen</Reserved>
          </div>
          <RunActionPanel
            item={item}
            history={history}
            revisions={revisions}
            stale={!!stale}
            review={review}
            publication={delivery}
            stat={stat}
            startError={startError}
            onEdit={editPlan}
            editing={editing}
            savedRevisionID={savedRevisionID}
            onOpen={openAndScroll}
            onRefresh={load}
            cancelSignal={cancelSignal}
            onRemovalPending={setRemovalPending}
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
                editing={editing}
                startEditing={() => setEditing(true)}
                latestInvocation={history.at(-1)}
                canEdit={item.state === "paused_gate" && !stale}
                stopEditing={() => setEditing(false)}
                saved={(revision) => { setOpen(revision); void load().then(() => setSavedRevisionID(revision.id)); }}
                refresh={load}
              />
            </div>
            <aside className="run-aside">
              <Facts item={item} />
              <section className="facts"><h2 className="section-title">Result</h2>
                <div className="fact"><span>publication</span><code>{delivery?.state || "—"}</code></div>
                <div className="fact"><span>commit</span><code title={item.result_commit}>{short(item.result_commit, 8) || "—"}</code></div>
                <div className="fact"><span>checks</span><code>{review?.checks?.ran ? review.checks.mandatory_passed ? "required passed" : "failed" : "—"}</code></div>
                <div className="fact"><span>reviewer</span><code>{review?.review?.verdict || "—"}</code></div>
                <div className="fact"><span>changes</span><code>{review?.diff?.stat_revision_id ? "diff.stat" : "—"}</code></div>
                {delivery?.pull_request?.url && <a className="run-pr-link" href={delivery.pull_request.url} target="_blank" rel="noreferrer">Open draft PR ↗</a>}
              </section>
              <section className="facts"><h2 className="section-title">Local resources</h2>
                <div className="fact"><span>worktree</span><code>{item.state === "created" ? "created on start" : terminalStates.has(item.state) ? resourceStatus(item.worktree?.state) : "in use by the run"}</code></div>
                <div className="fact"><span>branch</span><code>{item.state === "created" ? "created on start" : terminalStates.has(item.state) ? resourceStatus(item.branch_state) : "in use by the run"}</code></div>
              </section>
            </aside>
          </div>
        </>
      )}
    </Shell>
  );
}
