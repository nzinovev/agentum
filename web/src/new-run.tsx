import React, { useEffect, useState } from "react";
import { ApiError, createRun, project, startRun, type Project } from "./api";
import { Link, Loading, PageHeading, Problem, Shell } from "./common";
import type { Navigate } from "./main";
import { bytes } from "./util";
export function NewRunPage({
  projectID,
  navigate,
}: {
  projectID: string;
  navigate: Navigate;
}) {
  const [info, setInfo] = useState<Project | null>(null),
    [title, setTitle] = useState(""),
    [description, setDescription] = useState(""),
    [baseRef, setBaseRef] = useState(
      () =>
        localStorage.getItem("agentum.base_ref." + projectID) ||
        "refs/remotes/origin/main",
    ),
    [submitting, setSubmitting] = useState(false),
    [errors, setErrors] = useState<Record<string, string>>({}),
    [secret, setSecret] = useState<ApiError | null>(null),
    [projectLoading, setProjectLoading] = useState(true),
    [projectError, setProjectError] = useState<unknown>(null);
  const loadProject = () => {
    setProjectLoading(true);
    setProjectError(null);
    project(projectID)
      .then(setInfo)
      .catch(setProjectError)
      .finally(() => setProjectLoading(false));
  };
  useEffect(() => {
    setInfo(null);
    loadProject();
    setBaseRef(
      localStorage.getItem("agentum.base_ref." + projectID) ||
        "refs/remotes/origin/main",
    );
  }, [projectID]);
  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    if (submitting) return;
    const fieldErrors: Record<string, string> = {};
    if (!title.trim()) fieldErrors.title = "title is required";
    else if (bytes(title) > 200)
      fieldErrors.title = `title exceeds 200 bytes (got ${bytes(title)})`;
    if (!description.trim())
      fieldErrors.description = "description is required";
    else if (bytes(description) > 32768)
      fieldErrors.description = `description exceeds 32768 bytes (got ${bytes(description)})`;
    const normalizedBaseRef = baseRef.trim();
    if (!normalizedBaseRef) fieldErrors.base_ref = "base_ref is required";
    setErrors(fieldErrors);
    setSecret(null);
    if (Object.keys(fieldErrors).length) return;
    setSubmitting(true);
    try {
      const created = await createRun(projectID, title, description, normalizedBaseRef);
      localStorage.setItem("agentum.base_ref." + projectID, normalizedBaseRef);
      try {
        await startRun(created.id);
        sessionStorage.removeItem("agentum.start_error." + created.id);
      } catch (caught) {
        sessionStorage.setItem(
          "agentum.start_error." + created.id,
          caught instanceof Error ? caught.message : String(caught),
        );
      }
      navigate("/runs/" + created.id);
    } catch (caught) {
      const response =
        caught instanceof ApiError
          ? caught
          : new ApiError(0, "network", String(caught));
      if (response.status === 422) {
        setSecret(response);
        setErrors({
          [response.field === "title" ? "title" : "description"]:
            response.message,
        });
      } else if (response.field) {
        setErrors({ [response.field]: response.message });
      } else setErrors({ form: response.message });
    } finally {
      setSubmitting(false);
    }
  };
  const titleSize = bytes(title),
    descriptionSize = bytes(description);
  return (
    <Shell
      navigate={navigate}
      crumbs={[
        { label: "Projects", to: "/projects" },
        { label: info?.name || "project", to: "/projects/" + projectID },
        { label: "new run" },
      ]}
    >
      <PageHeading title="New run" />
      {projectLoading && <Loading label="Loading project…" />}
      {!!projectError && (
        <Problem
          error={projectError}
          title="This project could not be loaded."
          onRetry={loadProject}
          hint={"GET /api/v1/projects/" + projectID}
        />
      )}{" "}
      {info && (
        <form className="new-run-form" onSubmit={submit}>
          {secret && (
            <div className="inline-error" role="alert">
              <strong>
                A credential was found in the request{" "}
                <code>
                  {secret.status} {secret.code}
                </code>
              </strong>
              <pre>{secret.message}</pre>
              <p>Remove the value and submit again. Nothing was created.</p>
            </div>
          )}
          {errors.form && (
            <div className="inline-error" role="alert">
              <strong>Run could not be created</strong>
              <pre>{errors.form}</pre>
            </div>
          )}
          <label>
            <span className="label-row">
              <span>Title</span>
              <span
                className={"counter " + (titleSize > 200 ? "error-text" : "")}
              >
                {titleSize} / 200 bytes
              </span>
            </span>
            <input
              value={title}
              onChange={(event) => setTitle(event.target.value)}
              aria-invalid={!!errors.title}
              aria-describedby="title-error"
              required
            />
            {errors.title && (
              <small id="title-error" className="error-text" role="alert">
                {errors.title}
              </small>
            )}
          </label>
          <label>
            <span className="label-row">
              <span>Description</span>
              <span
                className={
                  "counter " + (descriptionSize > 32768 ? "error-text" : "")
                }
              >
                {(descriptionSize / 1024).toFixed(1)} KiB / 32 KiB
              </span>
            </span>
            <textarea
              className="mono"
              value={description}
              onChange={(event) => setDescription(event.target.value)}
              aria-invalid={!!errors.description}
              aria-describedby="desc-help desc-error"
              required
              rows={18}
            />
            <small id="desc-help">
              The request the agents work from: the problem, what to do,
              constraints, and when it is done. Agentum also uses it to choose the route.
            </small>
            {errors.description && (
              <small id="desc-error" className="error-text" role="alert">
                {errors.description}
              </small>
            )}
          </label>
          <label className="base-ref">
            Base ref
            <input
              className="mono"
              value={baseRef}
              onChange={(event) => setBaseRef(event.target.value)}
              aria-invalid={!!errors.base_ref}
              aria-describedby="base-help base-error"
              required
            />
            <small id="base-help">
              Branch or commit the run builds on. Prefilled with the last value
              used in this project.
            </small>
            {errors.base_ref && (
              <small id="base-error" className="error-text" role="alert">
                {errors.base_ref}
              </small>
            )}
          </label>
          <div className="form-actions">
            <button
              className="primary"
              type="submit"
              disabled={submitting}
              aria-busy={submitting}
            >
              {submitting ? "Creating…" : "Create and start"}
            </button>
            <Link to={"/projects/" + projectID} navigate={navigate}>
              Cancel
            </Link>
            <span>
              {submitting
                ? "Creating the run, then starting it. Repeat clicks are ignored."
                : "Creates the run and starts it. Agentum chooses a route for this request; the run page shows the route and why it was chosen."}
            </span>
          </div>
        </form>
      )}
    </Shell>
  );
}
