import React, { useEffect, useRef, useState } from "react";
import type { Run } from "./api";
import { exact, short } from "./util";

const stateLabel: Record<string, string> = {
  running: "Running", waiting: "Waiting for you", questions: "Questions for you",
  stopped: "Stopped here", failed: "Failed here", cancelled: "Cancelled here",
  rejected: "Rejected here", accepted: "Accepted", done: "Done",
  not_reached: "Not reached", not_taken: "Branch not taken",
};

const stateIcon: Record<string, string> = {
  running: "◌", waiting: "◆", questions: "◆", stopped: "■", failed: "×",
  cancelled: "■", rejected: "×", accepted: "✓", done: "✓",
  not_reached: "◌", not_taken: "⊘",
};

export function RouteBlock({ item, planRevision }: { item: Run; planRevision: number }) {
  const [expanded, setExpanded] = useState(false);
  const [transitionsOpen, setTransitionsOpen] = useState(false);
  const [copied, setCopied] = useState(false);
  const railRef = useRef<HTMLDivElement>(null);
  const scrolledRoute = useRef("");
  const graph = item.route_graph;
  const choosing = item.state === "running" && !item.route_source;
  const sourceLabel = item.route_source === "request" ? "Specified in request" : item.route_source === "fallback" ? "Fallback" : "Chosen by triage";
  const current = item.current_stage || (choosing ? "choosing route" : item.state === "created" ? "not started" : "—");
  const currentState = graph?.progress?.nodes[item.current_stage]?.state || "not_reached";
  const edgeCount = (from: string, to: string, condition = "") => graph?.progress?.edges.find((edge) => edge.from === from && edge.to === to && edge.condition === condition)?.count || 0;
  useEffect(() => {
    const routeKey = `${item.id}:${item.pipeline_pack}:${graph?.version || ""}`;
    if (!graph || !item.current_stage || scrolledRoute.current === routeKey) return;
    const rail = railRef.current;
    const currentCard = rail?.querySelector<HTMLElement>('[aria-current="step"]');
    if (!rail || !currentCard) return;
    rail.scrollLeft = Math.max(0, currentCard.offsetLeft - rail.offsetLeft - rail.clientWidth / 2 + currentCard.clientWidth / 2);
    scrolledRoute.current = routeKey;
  }, [item.id, item.pipeline_pack, item.current_stage, graph]);
  return <section className="route-block" aria-label="Route">
    {!item.route_source ? <div className="route-pending">
      {choosing ? <><span className="dot blue" /> <strong role="status">Choosing route…</strong><p>A read-only triage call is choosing a route for this request. Stages start after the choice.</p><div className="route-skeleton" aria-hidden="true"><span /><span /><span /><span /></div></>
        : <><strong>Route not chosen yet.</strong><p>When the run starts, Agentum reads the request and chooses a route.</p></>}
    </div> : <>
      <div className="route-row"><span className="route-label">Route</span><div className="route-value route-identity">
        <code className="route-name">{item.pipeline_pack}</code>
        {graph?.version && <code className="route-version">v{graph.version}</code>}
        {item.pipeline_pack_origin && <span className="route-chip">{item.pipeline_pack_origin}</span>}
        <span className={"route-chip source-" + item.route_source}>{item.route_source === "fallback" && "▲ "}{sourceLabel}</span>
        {item.route_decided_at && <time title={exact(item.route_decided_at)}>{item.route_source === "request" ? "set at creation " : "stored "}{new Date(item.route_decided_at).toLocaleTimeString("en", { hour12: false })}</time>}
        <span className={"route-now " + currentState} role="status">Now <code>{current}</code> {item.state === "running" && item.pause_requested_at ? "pause requested" : item.state}</span>
      </div></div>
      {item.route_fallback ? <div className="route-row"><span className="route-label">Why</span><div className="route-alert fallback" role="note"><strong>Triage did not return a usable route. The full route, {item.pipeline_pack}, is used.</strong><div><code>{item.route_fallback.code}</code></div><p id="route-reason" className={expanded ? "" : "route-clamped"}>{item.route_fallback.message}</p><button className="text-button" onClick={() => setExpanded(!expanded)} aria-expanded={expanded} aria-controls="route-reason">{expanded ? "Show less" : "Show full reason"}</button><button className="text-button" onClick={() => { void navigator.clipboard.writeText(item.route_fallback!.code + ": " + item.route_fallback!.message).then(() => { setCopied(true); setTimeout(() => setCopied(false), 1600); }); }}>{copied ? "Copied" : "Copy"}</button></div></div>
        : <div className="route-row"><span className="route-label">Why</span><div className="route-value route-reason"><p id="route-reason" className={expanded ? "" : "route-clamped"}>{item.route_reason}</p>{item.route_reason.length > 220 && <button className="text-button" onClick={() => setExpanded(!expanded)} aria-expanded={expanded} aria-controls="route-reason">{expanded ? "Show less" : "Show full reason"}</button>}</div></div>}
      {graph && <div className="route-row"><span className="route-label">Use for</span><p className="route-value route-use">{graph.description}</p></div>}
      <div className="route-row"><span className="route-label">Graph</span><div className="route-value route-graph-area">
        {item.route_resolve_error ? <div className="route-alert resolve-error" role="alert"><strong>The route graph cannot be read for this run.</strong> <code>{item.route_resolve_error.code}</code><p>{item.route_resolve_error.message}</p><small>The pack is read from base_commit {short(item.base_commit, 7)}. The graph appears on the next successful read.</small></div>
          : graph ? <>
            <div className="route-rail" ref={railRef} role="list" tabIndex={0} aria-label="Route stages">
              {graph.nodes.map((node, index) => {
                const progress = graph.progress?.nodes[node.id];
                const state = progress?.state || "not_reached";
                const passes = progress?.pass_sequences || [];
                const approval = graph.approvals.find((candidate) => candidate.stage === node.id);
                const plainNext = graph.nodes[index + 1]?.id;
                const revisionLabel = item.state === "paused_gate" && item.stop_reason === "plan_revision_drift" && approval?.unlocks === "source_write" && item.current_stage === node.id
                  ? `rev ${planRevision} · waiting for you` : "";
                const progressLabel = revisionLabel || (state === "running" && item.pause_requested_at ? "Running · pause requested" : stateLabel[state] || state);
                return <React.Fragment key={node.id}>
                  {index > 0 && <span className={graph.nodes[index - 1].transitions?.some((edge) => edge.to === node.id) ? "route-connector" : "route-connector hidden"} aria-hidden="true">{graph.approvals.some((candidate) => candidate.stage === graph.nodes[index - 1].id && !candidate.within_stage) ? "◆" : "→"}</span>}
                  <div className={"route-node " + state} role="listitem" tabIndex={0} title={node.id} aria-label={`${node.id}: ${stateLabel[state] || state}, ${passes.length} passes`} aria-current={item.current_stage === node.id ? "step" : undefined}>
                    <div className="route-node-title"><code>{node.id}</code>{node.id === graph.entry && <span>entry</span>}</div>
                    <div className="route-node-meta">{node.terminal ? "terminal" : `${node.role || "agent"} · ${approval?.within_stage ? "gate inside the stage" : node.gate.startsWith("human_") ? "human gate on exit" : node.gate}`}</div>
                    <div className="route-node-state"><span>{stateIcon[state] || "◌"}</span> {progressLabel}{state === "running" && passes.length > 1 ? ` · pass ${passes.length}` : ""}</div>
                    {!!passes.length && <small>{passes.length} {passes.length === 1 ? "pass" : "passes"} · {passes.map((pass) => `#${pass}`).join(" ")}</small>}
                    {progress?.steps?.length ? <div className="route-node-steps">{progress.steps.map((step) => <div className={"route-step " + step.state} key={step.id}><span>{step.state === "approved" || step.state === "done" ? "✓" : step.state === "waiting" ? "◆" : "◌"}</span> {step.label}</div>)}</div> : approval && <div className="route-node-gate">◆ approve {approval.artifact} → {approval.unlocks}</div>}
                    {node.final_review_gate && <div className="route-node-gate">◆ final human review · {item.state === "running" && item.previous_result_commit ? "returns here after checks" : state === "waiting" ? "waiting for you" : state === "accepted" ? "accepted" : state === "rejected" ? "rejected" : "not yet"}</div>}
                    {node.transitions?.filter((edge) => edge.condition || edge.to !== plainNext).map((edge) => <div className="route-node-edge" key={edge.to + edge.condition} title={edge.condition}>{graph.nodes.findIndex((candidate) => candidate.id === edge.to) < index ? "↩" : "→"} {edge.to} {edge.condition && <code>{edge.condition}</code>} {edgeCount(node.id, edge.to, edge.condition) > 0 && `×${edgeCount(node.id, edge.to, edge.condition)}`}</div>)}
                  </div>
                </React.Fragment>;
              })}
            </div>
            <div className="route-legend"><span>◆ on an arrow · gate between stages</span><span>◆ inside a card · gate within the stage</span><span>dashed · not reached or branch not taken</span><span>↩ repeat edge</span>{graph.nodes.length > 6 && <span>{graph.nodes.length} stages · scroll sideways for the rest</span>}<button className="text-button" onClick={() => setTransitionsOpen(!transitionsOpen)} aria-expanded={transitionsOpen} aria-controls="route-transitions">{transitionsOpen ? "Hide" : "Show"} transitions</button></div>
            {transitionsOpen && <div id="route-transitions" className="route-transitions"><p>{graph.description}</p><table><thead><tr><th>From</th><th>To</th><th>Condition</th><th>Taken</th></tr></thead><tbody>{graph.nodes.flatMap((node) => (node.transitions || []).map((edge) => <tr key={node.id + edge.to + edge.condition}><td>{node.id}</td><td>{edge.to}</td><td>{edge.condition || "always"}</td><td>{edgeCount(node.id, edge.to, edge.condition) || "not taken"}</td></tr>))}</tbody></table><small>entry {graph.entry} · fix cycles {graph.fix_cycles} · graph resolved from base_commit {short(item.base_commit, 7)}{item.route_triage_invocation_id && ` · triage invocation ${short(item.route_triage_invocation_id, 8)} in evidence`}</small></div>}
          </> : <p className="route-empty">The graph is available after the pack is resolved.</p>}
      </div></div>
    </>}
  </section>;
}
