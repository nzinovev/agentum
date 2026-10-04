export type Project = {
  id: string;
  name: string;
  repo_path: string;
  updated_at: string;
  previous_repo_path?: string;
  runs_rebound_to_new_checkout?: number;
  runs_awaiting_previous_checkout?: number;
};
export type Run = {
  id: string;
  project_id: string;
  title: string;
  description: string;
  state: string;
  current_stage: string;
  stop_reason: string;
  error: string;
  cancel_reason: string;
  open_questions?: string[];
  base_ref: string;
  base_commit: string;
  result_commit: string;
  branch: string;
  pipeline_pack: string;
  pipeline_pack_origin: string;
  created_at: string;
  updated_at: string;
};
export type Invocation = {
  id: string;
  sequence: number;
  stage: string;
  cycle: number;
  stop_reason?: string;
  resume_of?: string;
  started_at: string;
  finished_at?: string;
};
export type Artifact = {
  id: string;
  name: string;
  kind: string;
  content_hash: string;
  content_size: number;
  actor: string;
  is_current: boolean;
  created_at: string;
  prev_revision_id?: string;
};
export type Folder = {
  name: string;
  path: string;
  is_git: boolean;
  shallow: boolean;
  has_commits: boolean;
  registered_project_id: string;
};
export type Directory = { path: string; parent: string; directories: Folder[] };
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public field?: string,
  ) {
    super(message);
  }
}
export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let response: Response;
  try {
    response = await fetch("/api/v1" + path, {
      ...init,
      headers: {
        ...(init?.body ? { "Content-Type": "application/json" } : {}),
        ...init?.headers,
      },
    });
  } catch (error) {
    throw new ApiError(
      0,
      "network",
      error instanceof Error ? error.message : String(error),
    );
  }
  if (!response.ok) {
    let body: { error?: { code?: string; message?: string; field?: string } } =
      {};
    try {
      body = await response.json();
    } catch {}
    throw new ApiError(
      response.status,
      body.error?.code || "http_error",
      body.error?.message || response.statusText,
      body.error?.field,
    );
  }
  return response.json() as Promise<T>;
}
export const projects = () => request<Project[]>("/projects?limit=200");
export const project = (id: string) =>
  request<Project>("/projects/" + encodeURIComponent(id));
export const runs = (id: string, limit = 50, offset = 0) =>
  request<Run[]>(
    "/runs?project_id=" +
      encodeURIComponent(id) +
      "&limit=" +
      limit +
      "&offset=" +
      offset,
  );
export const run = (id: string) =>
  request<Run>("/runs/" + encodeURIComponent(id));
export const invocations = (id: string) =>
  request<Invocation[]>("/runs/" + encodeURIComponent(id) + "/invocations");
export const artifacts = (id: string) =>
  request<Artifact[]>("/runs/" + encodeURIComponent(id) + "/artifacts");
export const directory = (path?: string) =>
  request<Directory>(
    "/fs/dirs" + (path ? "?path=" + encodeURIComponent(path) : ""),
  );
export const createProject = (name: string, repo_path: string) =>
  request<Project>("/projects", {
    method: "POST",
    body: JSON.stringify({ name, repo_path }),
  });
export const createRun = (
  project_id: string,
  title: string,
  description: string,
  base_ref: string,
) =>
  request<Run>("/runs", {
    method: "POST",
    body: JSON.stringify({ project_id, title, description, base_ref }),
  });
export const startRun = (id: string) =>
  request<Run>("/runs/" + encodeURIComponent(id) + "/start", {
    method: "POST",
  });
export async function artifactContent(
  runID: string,
  revisionID: string,
): Promise<string> {
  let response: Response;
  try {
    response = await fetch(
      "/api/v1/runs/" +
        encodeURIComponent(runID) +
        "/artifacts/revisions/" +
        encodeURIComponent(revisionID) +
        "/content",
    );
  } catch (error) {
    throw new ApiError(
      0,
      "network",
      error instanceof Error ? error.message : String(error),
    );
  }
  if (!response.ok) {
    let body: { error?: { code?: string; message?: string } } = {};
    try {
      body = await response.json();
    } catch {}
    throw new ApiError(
      response.status,
      body.error?.code || "http_error",
      body.error?.message || response.statusText,
    );
  }
  return response.text();
}
