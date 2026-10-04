import React, { useState } from "react";
import type { Navigate } from "./main";
import { ApiError } from "./api";
import { stateIcon, stateLabel, stateTone } from "./util";
export function Link({
  to,
  navigate,
  children,
  className,
  title,
}: {
  to: string;
  navigate: Navigate;
  children: React.ReactNode;
  className?: string;
  title?: string;
}) {
  return (
    <a
      href={to}
      className={className}
      title={title}
      onClick={(event) => {
        if (event.ctrlKey || event.metaKey || event.shiftKey || event.altKey)
          return;
        event.preventDefault();
        navigate(to);
      }}
    >
      {children}
    </a>
  );
}
export function Shell({
  navigate,
  crumbs,
  poll,
  offline,
  children,
}: {
  navigate: Navigate;
  crumbs: { label: string; to?: string }[];
  poll?: string;
  offline?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="logo">
          <span className="logo-mark">A</span>
          <span>Agentum</span>
        </div>
        <nav>
          <Link to="/projects" navigate={navigate} className="side-active">
            Projects
          </Link>
        </nav>
        <div className="sidebar-bottom">
          <span>
            <i className={"dot " + (offline ? "amber" : "green")} />
            {offline ? "API not responding" : "API reachable"}
          </span>
          <span>{location.host || "localhost:8080"}</span>
        </div>
      </aside>
      <main className="main">
        <div className="topline">
          <nav className="crumbs" aria-label="Breadcrumb">
            {crumbs.map((crumb, index) => (
              <React.Fragment key={index}>
                {index > 0 && <span className="slash">/</span>}
                {crumb.to ? (
                  <Link to={crumb.to} navigate={navigate}>
                    {crumb.label}
                  </Link>
                ) : (
                  <span className="current">{crumb.label}</span>
                )}
              </React.Fragment>
            ))}
          </nav>
          {poll && (
            <span className="poll">
              <i className={"dot " + (offline ? "amber" : "green")} />
              {poll}
            </span>
          )}
        </div>
        {children}
      </main>
    </div>
  );
}
export function Badge({ state }: { state: string }) {
  return (
    <span className={"badge " + stateTone(state)} title={"state: " + state}>
      <span className="badge-icon">{stateIcon(state)}</span>
      {stateLabel(state)}
    </span>
  );
}
export function PageHeading({
  title,
  action,
}: {
  title: string;
  action?: React.ReactNode;
}) {
  return (
    <div className="page-heading">
      <h1>{title}</h1>
      {action}
    </div>
  );
}
export function Problem({
  error,
  title,
  onRetry,
  hint,
}: {
  error: unknown;
  title: string;
  onRetry?: () => void;
  hint?: string;
}) {
  const typed =
    error instanceof ApiError
      ? error
      : new ApiError(
          0,
          "network",
          error instanceof Error ? error.message : String(error),
        );
  return (
    <div
      role="alert"
      className={"problem " + (typed.status === 0 ? "human" : "fail")}
    >
      <div className="eyebrow">
        {typed.status === 0
          ? "Server unreachable"
          : "Request failed · " + typed.status}
        <code>{typed.code}</code>
      </div>
      <h2>{title}</h2>
      <p className="mono wrap">{typed.message}</p>
      {onRetry && (
        <div className="problem-bottom">
          <button className="secondary" onClick={onRetry}>
            Retry
          </button>
          <span>{hint}</span>
        </div>
      )}
    </div>
  );
}
export function Loading({ label = "Loading…" }: { label?: string }) {
  return (
    <div className="loading" aria-busy="true">
      <span>{label}</span>
      <div className="skeleton" />
      <div className="skeleton short" />
      <div className="skeleton" />
    </div>
  );
}
export function Copy({
  value,
  label = "Copy",
}: {
  value: string;
  label?: string;
}) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      className="copy"
      title={label}
      aria-label={label}
      onClick={() => {
        void navigator.clipboard.writeText(value).then(() => {
          setCopied(true);
          setTimeout(() => setCopied(false), 1500);
        });
      }}
    >
      {copied ? "✓" : "▢"}
    </button>
  );
}
export function Reserved({ children }: { children: React.ReactNode }) {
  return <div className="reserved">Reserved · {children}</div>;
}
