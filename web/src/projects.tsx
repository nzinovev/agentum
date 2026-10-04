import React, { useEffect, useRef, useState } from "react";
import {
  ApiError,
  createProject,
  directory,
  projects,
  type Directory,
  type Project,
} from "./api";
import { Copy, Link, Loading, PageHeading, Problem, Shell } from "./common";
import type { Navigate } from "./main";
import { exact, relative } from "./util";
function FolderPicker({
  close,
  useFolder,
  startPath,
}: {
  close: () => void;
  useFolder: (path: string) => void;
  startPath?: string;
}) {
  const [listing, setListing] = useState<Directory | null>(null),
    [home, setHome] = useState(""),
    [selected, setSelected] = useState(""),
    [error, setError] = useState<unknown>(null);
  const dialog = useRef<HTMLDivElement>(null);
  const open = (path?: string) => {
    setError(null);
    directory(path)
      .then((data) => {
        setListing(data);
        setSelected("");
      })
      .catch(setError);
  };
  useEffect(() => {
    directory()
      .then((root) => {
        setHome(root.path);
        setListing(root);
        if (
          startPath &&
          (startPath === root.path || startPath.startsWith(root.path + "/"))
        )
          open(startPath);
      })
      .catch(setError);
    dialog.current?.focus();
  }, []);
  useEffect(() => {
    const key = (event: KeyboardEvent) => {
      if (event.key === "Escape") close();
      if (event.key === "Tab") {
        const items = dialog.current?.querySelectorAll<HTMLElement>(
          'button,[tabindex="0"]',
        );
        if (!items?.length) return;
        const first = items[0],
          last = items[items.length - 1];
        if (event.shiftKey && document.activeElement === first) {
          event.preventDefault();
          last.focus();
        } else if (!event.shiftKey && document.activeElement === last) {
          event.preventDefault();
          first.focus();
        }
      }
    };
    document.addEventListener("keydown", key);
    return () => document.removeEventListener("keydown", key);
  }, [close]);
  const relative =
    home && listing?.path.startsWith(home)
      ? listing.path.slice(home.length)
      : "";
  const parts = relative.split("/").filter(Boolean);
  return (
    <div
      className="modal-scrim"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) close();
      }}
    >
      <div
        className="picker"
        role="dialog"
        aria-modal="true"
        aria-labelledby="pick-title"
        ref={dialog}
        tabIndex={-1}
      >
        <div className="picker-head">
          <h2 id="pick-title">Choose a repository folder</h2>
          <span className="mono muted">on localhost</span>
        </div>
        <div className="picker-nav">
          <button
            className="icon-button"
            disabled={!listing?.parent}
            aria-label="Parent folder"
            onClick={() => open(listing?.parent)}
          >
            ↑
          </button>
          <nav aria-label="Current folder" className="crumbs">
            <button onClick={() => open(home)} className="text-button">
              ~
            </button>
            {parts.map((part, index) => (
              <React.Fragment key={index}>
                <span>/</span>
                <button
                  className="text-button"
                  onClick={() =>
                    open(home + "/" + parts.slice(0, index + 1).join("/"))
                  }
                >
                  {part}
                </button>
              </React.Fragment>
            ))}
          </nav>
        </div>
        <div className="picker-list" role="listbox" aria-label="Folders">
          {error ? (
            <Problem
              error={error}
              title="This folder could not be loaded."
              onRetry={() => open(listing?.path || startPath)}
            />
          ) : !listing ? (
            <Loading label="Loading folders…" />
          ) : listing.directories.length === 0 ? (
            <p className="empty-folder">No folders here.</p>
          ) : (
            listing.directories.map((folder) => (
              <div
                key={folder.path}
                role="option"
                tabIndex={0}
                aria-selected={selected === folder.path}
                className={
                  "folder " + (selected === folder.path ? "selected" : "")
                }
                onClick={() => setSelected(folder.path)}
                onDoubleClick={() => open(folder.path)}
                onKeyDown={(event) => {
                  if (event.key === "Enter") {
                    event.preventDefault();
                    open(folder.path);
                  } else if (event.key === " ") {
                    event.preventDefault();
                    setSelected(folder.path);
                  }
                }}
              >
                <span
                  className={folder.is_git ? "folder-icon git" : "folder-icon"}
                >
                  ▱
                </span>
                <span className="truncate mono">{folder.name}</span>
                <span className="folder-tags">
                  {folder.is_git && (
                    <span className="tag git">
                      {folder.shallow
                        ? "shallow"
                        : !folder.has_commits
                          ? "no commits"
                          : "git"}
                    </span>
                  )}
                  {folder.registered_project_id && (
                    <span className="tag registered">registered</span>
                  )}
                </span>
              </div>
            ))
          )}
        </div>
        <div className="picker-foot">
          <code title={selected}>
            {selected || listing?.path || "Select a folder"}
          </code>
          <div>
            <button className="ghost" onClick={close}>
              Cancel
            </button>
            <button
              className="primary"
              disabled={!selected}
              onClick={() => useFolder(selected)}
            >
              Use this folder
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
export function ProjectsPage({ navigate }: { navigate: Navigate }) {
  const [items, setItems] = useState<Project[] | null>(null),
    [error, setError] = useState<unknown>(null),
    [formOpen, setFormOpen] = useState(false),
    [name, setName] = useState(""),
    [path, setPath] = useState(""),
    [submitting, setSubmitting] = useState(false),
    [formError, setFormError] = useState<ApiError | null>(null),
    [notice, setNotice] = useState<Project | null>(null),
    [picker, setPicker] = useState(false);
  const load = () => {
    setError(null);
    projects().then(setItems).catch(setError);
  };
  useEffect(load, []);
  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    if (submitting) return;
    setSubmitting(true);
    setFormError(null);
    try {
      const result = await createProject(name, path);
      setName("");
      setPath("");
      setFormOpen(false);
      if (result.previous_repo_path) setNotice(result);
      await projects().then(setItems);
    } catch (caught) {
      setFormError(
        caught instanceof ApiError
          ? caught
          : new ApiError(0, "network", String(caught)),
      );
    } finally {
      setSubmitting(false);
    }
  };
  const empty = items?.length === 0;
  return (
    <Shell
      navigate={navigate}
      crumbs={[{ label: "Projects" }]}
      poll={error ? "Request failed" : items ? "Updated just now" : "Loading…"}
      offline={error instanceof ApiError && error.status === 0}
    >
      <PageHeading
        title="Projects"
        action={
          !formOpen &&
          !empty && (
            <button className="primary" onClick={() => setFormOpen(true)}>
              Register project
            </button>
          )
        }
      />
      {notice && (
        <div className="notice" role="status">
          <div className="notice-content">
            <strong>Project registration updated</strong>
            <p className="mono">
              <span>previous_repo_path</span> {notice.previous_repo_path} →{" "}
              {notice.repo_path}
            </p>
            <p>
              {notice.runs_rebound_to_new_checkout
                ? `${notice.runs_rebound_to_new_checkout} unfinished runs moved to the new checkout.`
                : notice.runs_awaiting_previous_checkout
                  ? `${notice.runs_awaiting_previous_checkout} unfinished runs stayed with the previous checkout.`
                  : "Its runs are kept."}
            </p>
          </div>
          <button className="secondary" onClick={() => setNotice(null)}>
            Dismiss
          </button>
        </div>
      )}
      {empty && (
        <div className="empty-intro">
          <h2>No projects yet</h2>
          <p>
            Register a git repository on this machine to start runs against it.
          </p>
        </div>
      )}
      {(formOpen || empty) && (
        <form
          className="register-form"
          onSubmit={submit}
          aria-labelledby="reg-title"
        >
          <h2 id="reg-title">Register a project</h2>
          <p>
            Registering a repository that is already known updates it; its runs
            are kept.
          </p>
          <div className="register-fields">
            <label>
              Name
              <input
                required
                value={name}
                onChange={(event) => setName(event.target.value)}
                aria-invalid={formError?.field === "name"}
                aria-describedby="reg-error"
              />
            </label>
            <label>
              Repository path
              <span className="input-row">
                <input
                  className="mono"
                  required
                  value={path}
                  onChange={(event) => setPath(event.target.value)}
                  placeholder="/Users/you/src/repo"
                  aria-invalid={!!formError && formError.field !== "name"}
                  aria-describedby="path-help reg-error"
                />
                <button
                  type="button"
                  className="secondary"
                  onClick={() => setPicker(true)}
                >
                  ▱ &nbsp;Browse…
                </button>
              </span>
              <small id="path-help">
                Absolute path on the machine where Agentum runs. Type it or pick
                a folder.
              </small>
            </label>
          </div>
          {formError && (
            <div id="reg-error" role="alert" className="inline-error">
              <strong>
                Registration refused{" "}
                <code>
                  {formError.status || "network"} {formError.code}
                </code>
              </strong>
              <pre>{formError.message}</pre>
            </div>
          )}
          <div className="form-actions">
            <button
              type="submit"
              className="primary"
              disabled={submitting}
              aria-busy={submitting}
            >
              {submitting ? "Registering…" : "Register"}
            </button>
            {!empty && (
              <button
                type="button"
                className="ghost"
                onClick={() => {
                  setFormOpen(false);
                  setFormError(null);
                }}
              >
                Cancel
              </button>
            )}
          </div>
        </form>
      )}
      {!items && !error && <Loading label="Loading projects…" />}
      {!!error && (
        <Problem
          error={error}
          title="Projects could not be loaded."
          onRetry={load}
          hint="GET /api/v1/projects"
        />
      )}
      {items && items.length > 0 && (
        <div className="table projects-table" role="table">
          <div className="table-head project-grid" role="row">
            <span>name</span>
            <span>repo_path</span>
            <span className="right">updated</span>
          </div>
          {items.map((item) => (
            <Link
              to={"/projects/" + item.id}
              navigate={navigate}
              className={
                "table-row project-grid " +
                (notice?.id === item.id ? "tinted" : "")
              }
              key={item.id}
            >
              <strong>{item.name}</strong>
              <span className="truncate mono" title={item.repo_path}>
                {item.repo_path}
              </span>
              <time className="right mono" title={exact(item.updated_at)}>
                {relative(item.updated_at)}
              </time>
            </Link>
          ))}
        </div>
      )}
      {picker && (
        <FolderPicker
          close={() => setPicker(false)}
          useFolder={(selected) => {
            setPath(selected);
            setPicker(false);
          }}
          startPath={
            items?.[0]?.repo_path.split("/").slice(0, -1).join("/") || undefined
          }
        />
      )}
    </Shell>
  );
}
