import React, { useEffect, useState } from "react";
import { project, runs, type Project, type Run } from "./api";
import { Badge, Copy, Link, Loading, Problem, Shell } from "./common";
import type { Navigate } from "./main";
import { exact, relative, waitingStates } from "./util";
function RunGroup({
  name,
  items,
  navigate,
  waiting,
}: {
  name: string;
  items: Run[];
  navigate: Navigate;
  waiting?: boolean;
}) {
  if (items.length === 0) return null;
  return (
    <section className="run-group">
      <h2 className={"eyebrow " + (waiting ? "amber-text" : "")}>
        {name} <span>{items.length}</span>
      </h2>
      <div className={"table " + (waiting ? "waiting-table" : "")} role="table">
        {items.map((item) => {
          const context =
            item.state === "failed"
              ? ["error", item.error]
              : waitingStates.has(item.state) && item.stop_reason
                ? ["stop", item.stop_reason]
                : ["stage", item.current_stage || "—"];
          return (
            <Link
              key={item.id}
              to={"/runs/" + item.id}
              navigate={navigate}
              className="table-row run-grid"
            >
              <Badge state={item.state} />
              <strong className="truncate" title={item.title}>
                {item.title}
              </strong>
              <span className="run-context truncate">
                <span>{context[0]}</span>{" "}
                <code title={context[1]}>{context[1]}</code>
              </span>
              <time className="right mono" title={exact(item.updated_at)}>
                {relative(item.updated_at)}
              </time>
            </Link>
          );
        })}
      </div>
    </section>
  );
}
export function ProjectRunsPage({
  projectID,
  navigate,
}: {
  projectID: string;
  navigate: Navigate;
}) {
  const [info, setInfo] = useState<Project | null>(null),
    [items, setItems] = useState<Run[] | null>(null),
    [error, setError] = useState<unknown>(null),
    [stale, setStale] = useState(false),
    [loadingMore, setLoadingMore] = useState(false),
    [hasMore, setHasMore] = useState(false);
  const load = async (append = false) => {
    try {
      const pageCount = append
        ? 1
        : Math.max(1, Math.ceil((items?.length || 0) / 50));
      const offsets = append
        ? [items?.length || 0]
        : Array.from({ length: pageCount }, (_, index) => index * 50);
      const [projectData, pages] = await Promise.all([
        project(projectID),
        Promise.all(offsets.map((offset) => runs(projectID, 50, offset))),
      ]);
      const page = pages.flat();
      setInfo(projectData);
      const rows = append ? [...(items || []), ...page] : page;
      setItems(Array.from(new Map(rows.map((row) => [row.id, row])).values()));
      setHasMore(pages.at(-1)?.length === 50);
      setError(null);
      setStale(false);
    } catch (caught) {
      if (items) setStale(true);
      else setError(caught);
    } finally {
      setLoadingMore(false);
    }
  };
  useEffect(() => {
    setItems(null);
    setInfo(null);
    void load();
  }, [projectID]);
  useEffect(() => {
    const timer = setInterval(() => {
      void load();
    }, 5000);
    return () => clearInterval(timer);
  }, [projectID, items?.length]);
  const waiting = (items || []).filter((item) => waitingStates.has(item.state)),
    other = (items || []).filter((item) => !waitingStates.has(item.state));
  return (
    <Shell
      navigate={navigate}
      crumbs={[
        { label: "Projects", to: "/projects" },
        { label: info?.name || "project" },
      ]}
      poll={
        stale
          ? "Stale · retrying every 5s"
          : items
            ? "Updated just now · polling every 5s"
            : "Loading…"
      }
      offline={stale}
    >
      <div className="page-heading project-heading">
        <div>
          <h1>{info?.name || "Project runs"}</h1>
          {info && (
            <div className="project-path">
              <code title={info.repo_path}>{info.repo_path}</code>
              <Copy value={info.repo_path} label="Copy repository path" />
            </div>
          )}
        </div>
        {info && (
          <Link
            to={"/projects/" + projectID + "/new-run"}
            navigate={navigate}
            className="primary"
          >
            New run
          </Link>
        )}
      </div>
      {stale && (
        <div className="stale" role="status">
          The latest runs could not be loaded. Showing the last successful data.{" "}
          <button className="text-button" onClick={() => void load()}>
            Retry now
          </button>
        </div>
      )}
      {!items && !error && <Loading label="Loading runs…" />}
      {!!error && (
        <Problem
          error={error}
          title="This project could not be loaded."
          onRetry={() => void load()}
        />
      )}{" "}
      {items?.length === 0 && (
        <div className="empty-intro">
          <h2>No runs in this project yet</h2>
          <p>
            A run takes a request, plans it, implements it, reviews it and stops
            for your decisions along the way.
          </p>
          <Link
            to={"/projects/" + projectID + "/new-run"}
            navigate={navigate}
            className="primary"
          >
            Create the first run
          </Link>
        </div>
      )}
      {items && items.length > 0 && (
        <>
          <RunGroup
            name="Waiting for you"
            items={waiting}
            navigate={navigate}
            waiting
          />
          <RunGroup
            name={waiting.length ? "Other runs" : "Runs"}
            items={other}
            navigate={navigate}
          />
          <div className="list-footer">
            <span>Showing runs 1–{items.length}</span>
            {hasMore && (
              <button
                className="secondary"
                disabled={loadingMore}
                onClick={() => {
                  setLoadingMore(true);
                  void load(true);
                }}
              >
                {loadingMore ? "Loading…" : "Load next 50"}
              </button>
            )}
          </div>
        </>
      )}
    </Shell>
  );
}
