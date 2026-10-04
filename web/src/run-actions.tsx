import React, { useEffect, useRef, useState } from "react";
import { ApiError, artifactContent, postInvocation, postRun, type Artifact, type FinalReview, type Invocation, type Publication, type Run } from "./api";
import { Copy } from "./common";
import { short, stateTone, terminalStates } from "./util";

type ActionError = { status: number; code: string; message: string };
type DialogKind = "cancel" | "reject_plan" | "approve_result" | "reject_result" | "discard" | "cleanup";
type Notice = { tone: "work" | "human" | "ok" | "fail" | "neutral"; title: string; detail?: string };

function asActionError(caught: unknown): ActionError {
  return caught instanceof ApiError
    ? { status: caught.status, code: caught.code, message: caught.message }
    : { status: 0, code: "network", message: String(caught) };
}

function ErrorBox({ error, title }: { error: ActionError; title: string }) {
  return <div className="run-action-error" role="alert"><strong>{title}</strong><code>{error.status} {error.code} · {error.message}</code></div>;
}

function PublicationRow({ item, publication, disabled, onRetry, error, pending }: {
  item: Run; publication: Publication | null; disabled: boolean; onRetry: () => void;
  error: ActionError | null; pending: boolean;
}) {
  const state = pending ? "pending" : publication?.state || "unavailable";
  const labels: Record<string, string> = {
    pending: "Queued", publishing: "Publishing", published: "Draft PR open",
    not_attempted: "Not published", disabled: "Publication off", failed: "Failed · retryable",
    blocked: "Blocked", unavailable: "Status unknown",
  };
  return <div className="run-publication">
    <span className="run-mini-label">Publication</span>
    <div><strong>{labels[state] || state}</strong>{" "}
      {state === "published" && publication?.pull_request?.url ?
        <a href={publication.pull_request.url} target="_blank" rel="noreferrer">Draft PR #{publication.pull_request.number} · {publication.target?.owner}/{publication.target?.repository} ↗</a> : null}
      {state === "pending" && <p>Publication requested. Waiting for a worker to pick it up.</p>}
      {state === "publishing" && <p>Pushing the branch and creating the draft PR · attempt {publication?.attempts || 1}.</p>}
      {state === "published" && <p><code>pushed {short(publication?.published_commit || "", 7)} · PR state {publication?.pull_request?.state || "open"}</code></p>}
      {state === "not_attempted" && <p>The result is available on the local branch.</p>}
      {state === "disabled" && <p>Publication is off. The local result remains available.</p>}
      {state === "unavailable" && <p>The publication status could not be read. This does not mean publication is off.</p>}
      {(state === "failed" || state === "blocked") && <p>{publication?.last_error?.code}: {publication?.last_error?.message} · {publication?.attempts || 0} attempts{state === "blocked" ? ". The destination needs a change before retrying." : ""}</p>}
      {state !== "published" && <div className="run-branch-copy"><code title={item.branch}>{item.branch}</code><Copy value={item.branch} label="Copy branch" /></div>}
      {error && <ErrorBox title="Retry not accepted" error={error} />}
    </div>
    {(state === "failed" || state === "blocked") && <button className="secondary" disabled={disabled} onClick={onRetry}>Retry publication</button>}
  </div>;
}

function FinalReviewBlock({ item, review, stat, openRevision }: {
  item: Run; review: FinalReview | null; stat: string; openRevision: (name: string) => void;
}) {
  const checks = review?.checks;
  const results = checks?.results || [];
  const required = results.filter((result) => result.required);
  const passed = required.filter((result) => result.status === "pass");
  const statusNames: Record<string, string> = { pass: "Passed", fail: "Failed", timeout: "Timed out", error: "Error" };
  const findings = review?.review?.findings || [];
  const blocking = findings.filter((finding) => finding.severity === "blocker" || finding.severity === "major").length;
  const statSummary = stat.match(/(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?/);
  return <div className="run-review-grid">
    <div className="run-review-card"><h3>Checks <span>{passed.length} of {required.length} required passed</span></h3>
      {results.length ? results.map((result) => <div className="run-check-row" key={result.name}>
        <code title={result.name}>{result.name}</code><span>{result.required ? "required" : "optional"}</span>
        <strong className={result.status === "pass" ? "ok-text" : "fail-text"}>{checks?.ran ? statusNames[result.status] || result.status : "Not run"}</strong>
      </div>) : <p>No checks recorded.</p>}
      <small className={checks?.commit && item.result_commit && checks.commit !== item.result_commit ? "fail-text" : ""}>
        {checks?.ran ? `Ran on ${short(checks.commit, 7)} · result_commit is ${short(item.result_commit, 7)}` : "Checks not run"}
      </small>
    </div>
    <div className="run-review-card"><h3>Reviewer <span>{review?.review?.verdict || "No verdict"}</span></h3>
      <p>{blocking} blocking · {findings.length - blocking} non-blocking</p>
      {findings.map((finding) => <p key={finding.id}>{finding.severity}: {finding.detail}{finding.path ? ` · ${finding.path}${finding.line ? `:${finding.line}` : ""}` : ""}</p>)}
      <button className="artifact-link" onClick={() => openRevision("review/notes.md")}>Open review/notes.md</button>
    </div>
    <div className="run-review-card run-stat"><h3>Changes · diff.stat <span>{statSummary ? `${statSummary[1]} files · +${statSummary[2] || 0} −${statSummary[3] || 0}` : "—"} · {review?.diff?.stat_revision_id ? short(review.diff.stat_revision_id, 8) : "—"}</span></h3>
      <pre>{stat || "No diff.stat recorded."}</pre>
      <small>Read the code in the draft PR or local branch. Agentum does not display a code diff.</small>
    </div>
  </div>;
}

function ResourceCards({ item, disabled, pending, onDelete, error }: {
  item: Run; disabled: boolean; pending: string; onDelete: (kind: DialogKind) => void; error: ActionError | null;
}) {
  const worktree = item.worktree;
  const treeState = pending === "discard" && worktree?.state === "present" ? "removing" : worktree?.state || "not_created";
  const branchState = pending === "cleanup" && item.branch_state === "present" ? "removing" : item.branch_state || "not_created";
  const label = (state: string) => ({ present: "Kept", removing: "Removing…", removed: "Removed", not_created: "Not created" })[state] || state;
  return <div className="run-resources">
    <div className="run-resource-head"><strong>Local resources</strong><span>Kept until you delete them. Artifacts and the run record are never deleted.</span></div>
    <div className="run-resource-grid">
      <div className="run-review-card"><h3>Worktree <span>{label(treeState)}</span></h3>
        <div className="run-resource-fact"><span>path</span><code title={worktree?.path}>{worktree?.path || "—"}</code></div>
        <div className="run-resource-fact"><span>HEAD</span><code title={worktree?.head}>{short(worktree?.head || "", 10) || "—"}</code></div>
        <div className="run-resource-fact"><span>state</span><code>{worktree?.last_error?.code === "worktree_unreadable" || !worktree?.head && treeState === "present" ? "unreadable · changes unknown" : worktree?.dirty ? `${worktree.dirty_entries.length} uncommitted paths` : "clean"}</code></div>
        {treeState === "present" && <button className="danger-ghost" disabled={disabled} onClick={() => onDelete("discard")}>Delete worktree</button>}
        {treeState === "removing" && <small>Removal requested (202). Waiting for server confirmation.</small>}
        {treeState === "removed" && <small>Removed, confirmed by the server.</small>}
      </div>
      <div className="run-review-card"><h3>Branch <span>{label(branchState)}</span></h3>
        <div className="run-resource-fact"><span>name</span><code title={item.branch}>{item.branch}</code></div>
        <div className="run-resource-fact"><span>tip</span><code title={item.branch_tip}>{short(item.branch_tip || "", 10) || "—"}</code></div>
        {branchState === "present" && <button className="danger-ghost" disabled={disabled || treeState === "present" || treeState === "removing"} onClick={() => onDelete("cleanup")}>Delete branch</button>}
        {branchState === "present" && (treeState === "present" || treeState === "removing") && <small>Delete the worktree first.</small>}
        {branchState === "removing" && <small>Removal requested (202). Waiting for server confirmation.</small>}
        {branchState === "removed" && <small>Removed, confirmed by the server.</small>}
      </div>
    </div>
    {(error || item.worktree?.last_error || item.branch_last_error) && <ErrorBox title={item.worktree?.last_error?.code === "worktree_unreadable" ? "Worktree could not be read" : "Removal was refused"} error={error || { status: 0, ...(item.worktree?.last_error || item.branch_last_error)! }} />}
  </div>;
}

export function RunActionPanel({ item, history, revisions, stale, review, publication, stat, startError, now, editing, savedRevisionID = "", onEdit, onOpen, onRefresh, cancelSignal = 0, onRemovalPending }: {
  item: Run; history: Invocation[]; revisions: Artifact[]; stale: boolean; review: FinalReview | null;
  publication: Publication | null; stat: string; startError: string; now: number; editing: boolean; savedRevisionID?: string;
  onEdit: () => void; onOpen: (revision: Artifact) => void; onRefresh: () => Promise<void>;
  cancelSignal?: number; onRemovalPending?: (pending: boolean) => void;
}) {
  const [changesOpen, setChangesOpen] = useState(false);
  const [changesDraft, setChangesDraft] = useState("");
  const [answerDraft, setAnswerDraft] = useState("");
  const [noteDraft, setNoteDraft] = useState("");
  const [reconcileMode, setReconcileMode] = useState("resume_session");
  const [confirmLoss, setConfirmLoss] = useState(false);
  const [dialog, setDialog] = useState<DialogKind | null>(null);
  const [dialogCheck, setDialogCheck] = useState(false);
  const [dialogError, setDialogError] = useState<ActionError | null>(null);
  const [formError, setFormError] = useState<ActionError | null>(null);
  const [publicationError, setPublicationError] = useState<ActionError | null>(null);
  const [resourceError, setResourceError] = useState<ActionError | null>(null);
  const [pendingRemoval, setPendingRemoval] = useState("");
  const [pendingPublish, setPendingPublish] = useState(false);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<Notice | null>(null);
  const [reviewOpen, setReviewOpen] = useState(false);
  const [planContent, setPlanContent] = useState("");
  const latest = history.at(-1);
  const plan = revisions.find((revision) => revision.name === "plan/plan.md" && revision.is_current);
  const planRevision = revisions.filter((revision) => revision.name === "plan/plan.md").length;
  const remaining = Math.max(0, (item.plan_edits?.max || 0) - (item.plan_edits?.used || 0));
  const disabled = busy || stale || !!notice && notice.tone === "work";
  const previousState = useRef(item.state);
  const previousSavedRevision = useRef(savedRevisionID);
  useEffect(() => {
    if (savedRevisionID && savedRevisionID !== previousSavedRevision.current) {
      previousSavedRevision.current = savedRevisionID;
      setNotice({ tone: "ok", title: `Saved as rev ${planRevision}. The run is still at the gate.`, detail: `revision_id ${savedRevisionID}` });
    }
  }, [savedRevisionID, planRevision]);
  const previousCancelSignal = useRef(cancelSignal);
  useEffect(() => {
    if (cancelSignal !== previousCancelSignal.current) {
      previousCancelSignal.current = cancelSignal;
      setDialog("cancel"); setDialogError(null);
    }
  }, [cancelSignal]);
  useEffect(() => {
    if (previousState.current !== item.state) {
      previousState.current = item.state;
      setChangesOpen(false); setFormError(null); setNotice(null);
      setReconcileMode("resume_session"); setConfirmLoss(false);
    }
  }, [item.state]);
  useEffect(() => {
    if (!plan) { setPlanContent(""); return; }
    let active = true;
    artifactContent(item.id, plan.id).then((content) => { if (active) setPlanContent(content); }).catch(() => {});
    return () => { active = false; };
  }, [item.id, plan?.id]);
  useEffect(() => {
    if (stale) return;
    if (pendingRemoval === "discard" && (item.worktree?.state === "removed" || item.worktree?.last_error)) {
      if (item.worktree?.last_error) setResourceError({ status: 0, ...item.worktree.last_error });
      setNotice(null);
      setPendingRemoval("");
    }
    if (pendingRemoval === "cleanup" && (item.branch_state === "removed" || item.branch_last_error)) {
      if (item.branch_last_error) setResourceError({ status: 0, ...item.branch_last_error });
      setNotice(null);
      setPendingRemoval("");
    }
  }, [pendingRemoval, stale, item.worktree?.state, item.worktree?.last_error, item.branch_state, item.branch_last_error]);
  useEffect(() => { onRemovalPending?.(!!pendingRemoval); }, [pendingRemoval, onRemovalPending]);
  useEffect(() => { if (publication?.state !== "pending") setPendingPublish(false); }, [publication?.state]);
  useEffect(() => {
    if (!dialog) return;
    const handleKey = (event: KeyboardEvent) => {
      if (event.key === "Escape" && !busy) setDialog(null);
      if (event.key === "Tab") {
        const focusable = Array.from(document.querySelectorAll<HTMLElement>(".run-dialog button:not(:disabled), .run-dialog input:not(:disabled)"));
        if (!focusable.length) return;
        const index = focusable.indexOf(document.activeElement as HTMLElement);
        if (event.shiftKey && index <= 0) { event.preventDefault(); focusable.at(-1)?.focus(); }
        else if (!event.shiftKey && index === focusable.length - 1) { event.preventDefault(); focusable[0].focus(); }
      }
    };
    document.addEventListener("keydown", handleKey);
    document.querySelector<HTMLElement>(".run-dialog button")?.focus();
    return () => document.removeEventListener("keydown", handleKey);
  }, [dialog, busy]);
  const openRevision = (name: string) => { const revision = revisions.find((candidate) => candidate.name === name && candidate.is_current); if (revision) onOpen(revision); };
  const perform = async (action: string, request: () => Promise<unknown>) => {
    setBusy(true); setFormError(null); setNotice(null);
    try {
      await request();
      setNotice({ tone: "work", title: `${action} accepted · waiting for the run to update…` });
      await onRefresh();
    } catch (caught) {
      const failure = asActionError(caught);
      if (failure.code === "illegal_transition") {
        setFormError(failure);
        setNotice({ tone: "human", title: `${action} was not accepted.`, detail: `${failure.status} ${failure.code} · ${failure.message}` });
        await onRefresh();
      } else if (action === "Reconcile" && (failure.status === 409 || failure.status === 400)) {
        setReconcileMode("resume_session"); setConfirmLoss(false);
        setNotice({ tone: "human", title: "The worktree changed since this page loaded. Nothing was applied.", detail: `${failure.status} ${failure.code} · ${failure.message}` });
        setFormError(failure); await onRefresh();
      } else if (failure.code === "edit_budget_exhausted") {
        setFormError(failure); await onRefresh();
      } else if ((action === "Plan approval" || action === "Request changes") && (failure.status === 409 || failure.status === 428)) {
        setNotice({ tone: "human", title: "Plan changed. Review the current revision before trying again.", detail: `${failure.status} ${failure.code} · ${failure.message}` });
        await onRefresh();
        setFormError(failure);
      } else if (action === "Plan approval") {
        setNotice({ tone: "fail", title: "Approval failed. The run is still at the gate.", detail: `${failure.status} ${failure.code} · ${failure.message}` });
      } else setFormError(failure);
    } finally { setBusy(false); }
  };
  const submitDialog = async () => {
    if (!dialog) return;
    setBusy(true); setDialogError(null); setResourceError(null);
    try {
      let requestedRemoval = "";
      if (dialog === "approve_result") {
        if (!latest) throw new Error("No invocation is available");
        await postInvocation(item.id, latest.id, "approve");
      } else if (dialog === "discard") {
        await postRun(item.id, "worktree/discard", item.worktree?.head
          ? { expected_head: item.worktree.head, ...(item.worktree.dirty ? { discard_uncommitted: dialogCheck } : {}) }
          : { discard_unreadable: true, discard_uncommitted: dialogCheck });
        requestedRemoval = "discard";
      } else if (dialog === "cleanup") {
        await postRun(item.id, "cleanup"); requestedRemoval = "cleanup";
      } else await postRun(item.id, dialog === "cancel" ? "cancel" : "reject");
      setDialog(null);
      setNotice({ tone: "work", title: "Request accepted · waiting for the server to confirm…" });
      await onRefresh();
      if (requestedRemoval) setPendingRemoval(requestedRemoval);
    } catch (caught) {
      const failure = asActionError(caught);
      setDialogError(failure);
      if (dialog === "discard" || dialog === "cleanup") setResourceError(failure);
      if (failure.code === "illegal_transition") await onRefresh();
    } finally { setBusy(false); }
  };
  const retryPublication = async () => {
    setBusy(true); setPublicationError(null);
    try { await postRun(item.id, "publish"); setPendingPublish(true); await onRefresh(); }
    catch (caught) { setPublicationError(asActionError(caught)); }
    finally { setBusy(false); }
  };
  const state = item.state;
  const reason = item.stop_reason;
  const titles: Record<string, string> = {
    created: startError ? "The run was created, but starting it failed." : "The run is created and has not started.",
    running: latest ? `Invocation #${latest.sequence} · ${latest.stage} · cycle ${latest.cycle}` : "The run is starting.",
    paused_gate: "The plan is ready for approval.",
    paused_open_questions: `The agent asked ${(item.open_questions || []).length} questions in ${item.current_stage || "plan"}.`,
    paused_user_stop: reason === "base_not_on_target" ? "Base is not on the target branch" : reason === "worktree_uncommitted_changes" ? "Uncommitted changes in the worktree" : reason.replaceAll("_", " "),
    awaiting_final_review: "The work is ready for final review.", done: "Accepted at final review.",
    failed: `The run failed${item.current_stage ? " in " + item.current_stage : ""}.`,
    cancelled: item.cancel_reason === "rejected_at_plan" ? "The plan was rejected." : item.cancel_reason === "rejected_at_final_review" ? "The result was rejected." : item.base_commit ? `The run was cancelled during ${item.current_stage || "execution"}.` : "The run was cancelled before it started.",
  };
  const body: Record<string, string> = {
    created: startError || "Starting resolves the base and creates the run branch and worktree.",
    running: "The agent is working. This page updates every 3 seconds.",
    paused_gate: "No source code changes until the plan is approved.",
    paused_open_questions: "The stage is blocked until the questions are answered.",
    paused_user_stop: reason === "base_not_on_target" ? "The base carries commits that the publication target branch does not have." : "Review the reason and choose how to continue.",
    awaiting_final_review: `Result commit ${short(item.result_commit, 7)} on ${item.branch}.`,
    done: `Result commit ${short(item.result_commit, 7)}. Local resources are kept.`,
    failed: "The worktree and branch are kept until you delete them.",
    cancelled: item.base_commit ? "The worktree and branch are kept until you delete them." : "No worktree or branch was created.",
  };
  const dialogTitles: Record<DialogKind, string> = { cancel: "Cancel this run?", reject_plan: "Reject the plan and end the run?", approve_result: "Accept the result?", reject_result: "Reject the result?", discard: "Delete the worktree?", cleanup: "Delete the branch?" };
  const dialogActions: Record<DialogKind, string> = { cancel: "Cancel run", reject_plan: "Reject run", approve_result: "Approve result", reject_result: "Reject run", discard: "Delete worktree", cleanup: "Delete branch" };
  return <>
    <section className={"stop-panel run-action-panel " + stateTone(state)} aria-busy={busy}>
      <div><div className="eyebrow">{terminalStates.has(state) ? state : state === "running" ? "Working · no action needed" : "Waiting for you"}{reason && state.startsWith("paused") && <code>{reason}</code>}</div>
        <h2>{titles[state] || state}</h2><p>{body[state] || ""}</p>
        {stale && <div className="run-notice human"><strong>Decisions are disabled while the data is stale.</strong></div>}
        {!stale && notice && <div className={"run-notice " + notice.tone}><strong>{notice.title}</strong>{notice.detail && <code>{notice.detail}</code>}</div>}
        {state === "paused_gate" && <>
          <div className="run-plan-meta"><code>plan/plan.md</code><span>rev {planRevision}</span><code title={plan?.id}>{short(plan?.id || "", 8)}</code><span>{plan?.actor === "human" ? "edited by you" : "agent"}</span><em>Request changes · {remaining} of {item.plan_edits?.max || 0} left</em></div>
          <div className="run-plan-excerpt"><pre>{planContent || "Loading plan…"}</pre></div>
          <button className="artifact-link" onClick={() => openRevision("plan/plan.md")}>Read the full plan below ↓</button>
        </>}
        {state === "paused_open_questions" && <ol className="questions">{(item.open_questions || []).map((question, index) => <li key={index}>{question}</li>)}</ol>}
        {state === "failed" && item.error && <pre className="failure-detail">{item.error}</pre>}
      </div>
      <div className="run-action-side">
        {state === "paused_gate" && <>
          <button className="primary" disabled={disabled || editing || !plan || !latest} onClick={() => latest && plan && void perform("Plan approval", () => postInvocation(item.id, latest.id, "advance", { expected_revision_id: plan.id }))}>Approve plan · rev {planRevision}</button>
          <button className="secondary" aria-expanded={changesOpen} disabled={disabled || remaining === 0} onClick={() => setChangesOpen(!changesOpen)}>Request changes{remaining === 0 ? " · 0 left" : ""}</button>
          <button className="secondary" disabled={disabled || !plan} onClick={onEdit}>Edit plan</button>
          <button className="danger-ghost" disabled={disabled} onClick={() => { setDialog("reject_plan"); setDialogError(null); }}>Reject run</button>
        </>}
        {state === "paused_open_questions" && <button className="primary" disabled={disabled || !answerDraft.trim() || !latest} onClick={() => latest && void perform("Answer", () => postInvocation(item.id, latest.id, "continue", { text: answerDraft }))}>Continue</button>}
        {state === "paused_user_stop" && reason !== "worktree_uncommitted_changes" && <button className="primary" disabled={disabled} onClick={() => void perform("Continue", () => latest ? postInvocation(item.id, latest.id, "continue", noteDraft.trim() ? { text: noteDraft } : undefined) : postRun(item.id, "continue", noteDraft.trim() ? { text: noteDraft } : undefined))}>{reason === "base_not_on_target" ? "Continue after fixing" : "Continue"}</button>}
        {state === "paused_user_stop" && reason === "worktree_uncommitted_changes" && <button className={reconcileMode === "discard_to_checkpoint" ? "danger-ghost" : "primary"} disabled={disabled || !item.worktree?.head || reconcileMode === "discard_to_checkpoint" && !confirmLoss} onClick={() => void perform("Reconcile", () => postRun(item.id, "worktree/reconcile", { mode: reconcileMode, expected_head: item.worktree?.head, ...(reconcileMode === "discard_to_checkpoint" ? { confirm_uncommitted_loss: true } : {}) }))}>{reconcileMode === "discard_to_checkpoint" ? "Discard and resume" : reconcileMode === "keep_as_checkpoint" ? "Keep and resume" : "Resume session"}</button>}
        {state === "awaiting_final_review" && <><button className="primary" disabled={disabled || !latest} onClick={() => { setDialog("approve_result"); setDialogError(null); }}>Approve result</button><button className="danger-ghost" disabled={disabled} onClick={() => { setDialog("reject_result"); setDialogError(null); }}>Reject run</button><small>Does not depend on publication.</small></>}
        {state === "running" && latest && <div className="live-time"><strong>{Math.max(0, Math.floor((now - Date.parse(latest.started_at)) / 1000))}s</strong><span>since {new Date(latest.started_at).toLocaleTimeString("en", { hour12: false })}</span></div>}
      </div>
      {state === "paused_gate" && changesOpen && <div className="run-action-expansion"><div className="run-form-label"><label htmlFor="plan-changes">What should the planner change?</label><span>request {(item.plan_edits?.used || 0) + 1} of {item.plan_edits?.max || 0} · targets rev {planRevision}</span></div>
        <textarea id="plan-changes" value={changesDraft} onChange={(event) => setChangesDraft(event.target.value)} aria-invalid={!!formError} disabled={disabled} />
        <div className="run-form-footer"><button className="primary" disabled={disabled || !changesDraft.trim() || !latest || !plan || remaining === 0} onClick={() => latest && plan && void perform("Request changes", () => postInvocation(item.id, latest.id, "ask-to-edit", { text: changesDraft, target_revision_id: plan.id }))}>Send to planner</button><button className="secondary" onClick={() => setChangesOpen(false)}>Cancel</button><span>Saving remarks asks the planner to create a new revision.</span></div>
        {remaining === 0 && <small>Budget spent · {item.plan_edits?.used || 0} of {item.plan_edits?.max || 0} used. Approve, edit, or reject the current plan.</small>}
        {formError && <ErrorBox title="Request changes failed" error={formError} />}
      </div>}
      {state === "paused_open_questions" && <div className="run-action-expansion"><label htmlFor="agent-answer">Your answer</label><textarea id="agent-answer" value={answerDraft} onChange={(event) => setAnswerDraft(event.target.value)} aria-invalid={!!formError} disabled={disabled} /><small>{new TextEncoder().encode(answerDraft).length} bytes</small>{formError && <ErrorBox title="Answer was not sent" error={formError} />}</div>}
      {state === "paused_user_stop" && reason !== "worktree_uncommitted_changes" && <div className="run-action-expansion">{reason === "base_not_on_target" && <><h3>Fix this outside Agentum, then continue</h3><p>Base commit <code>{item.base_commit}</code> must be on <code>{item.publication_target_ref || "the publication target branch"}</code>.</p><ol><li>Push the missing base commits to the target, or create a new run from the target.</li><li>Fetch the remote target in the local checkout.</li><li>Continue to re-check the base.</li></ol></>}<label htmlFor="stop-note">Note for the agent (optional)</label><textarea id="stop-note" value={noteDraft} onChange={(event) => setNoteDraft(event.target.value)} disabled={disabled} />{formError && <ErrorBox title="Continue failed" error={formError} />}</div>}
      {state === "paused_user_stop" && reason === "worktree_uncommitted_changes" && <div className="run-action-expansion"><h3>Worktree recovery</h3><div className="run-resource-fact"><span>HEAD</span><code>{item.worktree?.head}</code></div><div className="run-resource-fact"><span>Restore target</span><code>{item.worktree?.restore_target?.label} · {item.worktree?.restore_target?.commit}</code></div><div className="run-dirty-list">{item.worktree?.dirty_entries.map((entry) => <div key={entry.path}><code>{entry.status}</code> {entry.path}</div>)}</div>
        <div className="run-reconcile-options">{[["resume_session", "Resume session", "Continue over the files as they are."], ["keep_as_checkpoint", "Keep as checkpoint", "Commit current changes before continuing."], ["discard_to_checkpoint", "Discard to checkpoint", "Lose the uncommitted paths and restore the checkpoint."]].map(([mode, label, description]) => <label key={mode} className={reconcileMode === mode ? mode === "discard_to_checkpoint" ? "selected danger-selected" : "selected" : ""}><input type="radio" name="reconcile" value={mode} checked={reconcileMode === mode} onChange={() => { setReconcileMode(mode); setConfirmLoss(false); }} disabled={disabled} /><strong>{label}</strong><span>{description}</span><code>{mode}</code></label>)}</div>
        {reconcileMode === "discard_to_checkpoint" && <label className="run-loss-check"><input type="checkbox" checked={confirmLoss} onChange={(event) => setConfirmLoss(event.target.checked)} />I understand that the {item.worktree?.dirty_entries.length || 0} uncommitted paths above will be lost.</label>}
        {formError && <ErrorBox title="The worktree changed. Nothing was applied." error={formError} />}
      </div>}
      {(state === "awaiting_final_review" || state === "done" || state === "cancelled" && item.cancel_reason === "rejected_at_final_review") && <div className="run-action-expansion">{state !== "awaiting_final_review" && <button className="secondary" aria-expanded={reviewOpen} onClick={() => setReviewOpen(!reviewOpen)}>{reviewOpen ? "Hide" : "Show"} final review: checks, reviewer, changes</button>}{(state === "awaiting_final_review" || reviewOpen) && <FinalReviewBlock item={item} review={review} stat={stat} openRevision={openRevision} />}</div>}
      {(state === "awaiting_final_review" || state === "done") && <div className="run-action-expansion"><PublicationRow item={item} publication={publication} disabled={disabled} onRetry={() => void retryPublication()} error={publicationError} pending={pendingPublish} /></div>}
      {terminalStates.has(state) && <div className="run-action-expansion"><ResourceCards item={item} disabled={disabled} pending={pendingRemoval} error={resourceError} onDelete={(kind) => { setDialog(kind); setDialogCheck(false); setDialogError(null); }} /></div>}
    </section>
    {dialog && <div className="run-dialog-scrim" role="presentation"><div className="run-dialog" role="dialog" aria-modal="true" aria-label={dialogTitles[dialog]} aria-busy={busy}>
      <h2>{dialogTitles[dialog]}</h2><p>Run “{item.title}”</p>
      {(dialog === "cancel" || dialog.startsWith("reject")) && <p>The run ends. Its worktree and branch stay until you delete them.</p>}
      {dialog === "approve_result" && <p>Publication continues on its own.</p>}
      {dialog === "discard" && <p>{item.worktree?.head ? "Delete this local worktree. The branch and artifacts remain." : "The worktree cannot be read. Delete its files and Git registration without checking HEAD or uncommitted changes. The branch and artifacts remain."}</p>}
      {dialog === "cleanup" && <p>Delete the local branch after its worktree has been removed.</p>}
      <div className="run-dialog-facts"><code>{dialog === "discard" ? item.worktree?.path : dialog === "reject_plan" ? `plan/plan.md · rev ${planRevision}` : item.branch}</code><code>{dialog === "discard" ? item.worktree?.head : dialog === "reject_plan" ? plan?.id : item.result_commit || item.branch_tip}</code></div>
      {dialog === "discard" && !!item.worktree?.dirty_entries.length && <div className="run-dirty-list">{item.worktree.dirty_entries.map((entry) => <div key={entry.path}><code>{entry.status}</code> {entry.path}</div>)}</div>}
      {dialog === "discard" && (item.worktree?.dirty || !item.worktree?.head) && <label className="run-loss-check"><input type="checkbox" checked={dialogCheck} onChange={(event) => setDialogCheck(event.target.checked)} />{item.worktree?.head ? `I understand that ${item.worktree.dirty_entries.length} uncommitted paths will be lost.` : "I understand that all files in this unreadable worktree will be lost."}</label>}
      {dialogError && <ErrorBox title="Request was refused" error={dialogError} />}
      <div className="run-dialog-footer"><button className="secondary" disabled={busy} onClick={() => setDialog(null)}>Keep</button><button className={dialog === "approve_result" ? "primary" : "danger-ghost"} disabled={busy || stale || dialog === "discard" && (!!item.worktree?.dirty || !item.worktree?.head) && !dialogCheck} onClick={() => void submitDialog()}>{busy ? "Requesting…" : dialogActions[dialog]}</button></div>
    </div></div>}
  </>;
}
