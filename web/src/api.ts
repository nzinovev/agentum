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
  plan_edits?: { used: number; max: number };
  worktree?: {
    state: "not_created" | "present" | "removing" | "removed";
    path?: string;
    head?: string;
    dirty: boolean;
    dirty_entries: { status: string; path: string }[];
    restore_target?: { label: string; commit: string };
    last_error?: { code: string; message: string };
  };
  branch_state?: "not_created" | "present" | "removing" | "removed";
  branch_tip?: string;
  branch_last_error?: { code: string; message: string };
  publication_target_ref?: string;
  base_ref: string;
  base_commit: string;
  result_commit: string;
  branch: string;
  pipeline_pack: string;
  pipeline_pack_origin: string;
  route_source: "request" | "triage" | "fallback" | "";
  route_reason: string;
  route_decided_at?: string;
  route_triage_invocation_id?: string;
  route_fallback?: { code: string; message: string };
  route_resolve_error?: { code: string; message: string };
  route_graph?: RouteGraph;
  created_at: string;
  updated_at: string;
};
export type RouteGraph = {
  description: string;
  version: string;
  entry: string;
  fix_cycles: number;
  approvals: { name: string; stage: string; artifact: string; unlocks: string; within_stage?: boolean }[];
  nodes: { id: string; gate: string; role?: string; terminal: boolean; transitions?: { to: string; condition?: string }[] }[];
};
export type Publication = {
  state: string;
  attempts?: number;
  published_commit?: string;
  pull_request?: { number: number; url: string; state: string };
  target?: { owner: string; repository: string; base_branch: string };
  last_error?: { code: string; message: string };
};
export type FinalReview = {
  git?: { branch: string; base_commit: string; result_commit?: string };
  diff?: { stat_revision_id?: string };
  review?: { verdict: string; findings: { id: string; severity: string; detail: string; path?: string; line?: number }[] };
  checks?: { commit: string; ran: boolean; mandatory_passed: boolean; results: { name: string; required: boolean; status: string }[] };
  publication?: Publication;
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
const runPath = (id: string) => "/runs/" + encodeURIComponent(id);
export const postRun = (id: string, action: string, body?: object) =>
  request<Run>(runPath(id) + "/" + action, {
    method: "POST",
    ...(body ? { body: JSON.stringify(body) } : {}),
  });
export const postInvocation = (id: string, invocationID: string, action: string, body?: object) =>
  request<Run>(runPath(id) + "/invocations/" + encodeURIComponent(invocationID) + "/" + action, {
    method: "POST",
    ...(body ? { body: JSON.stringify(body) } : {}),
  });
export const savePlan = (id: string, invocationID: string, path: string, content: string, expectedRevisionID: string) =>
  request<Artifact>(runPath(id) + "/invocations/" + encodeURIComponent(invocationID) + "/artifacts/" + path.split("/").map(encodeURIComponent).join("/"), {
    method: "PUT",
    body: JSON.stringify({ content, expected_revision_id: expectedRevisionID }),
  });
export const finalReview = (id: string) => request<FinalReview>(runPath(id) + "/final-review");
export const publication = (id: string) => request<Publication>(runPath(id) + "/publication");
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
