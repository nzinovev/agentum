export function relative(date: string): string {
  const diff = Date.now() - new Date(date).getTime();
  if (!Number.isFinite(diff)) return "—";
  if (diff < 60000) return "just now";
  if (diff < 3600000) return Math.floor(diff / 60000) + "m ago";
  if (diff < 86400000) return Math.floor(diff / 3600000) + "h ago";
  if (diff < 172800000) return "Yesterday";
  return new Date(date).toLocaleDateString("en", {
    month: "short",
    day: "numeric",
  });
}
export function exact(date: string): string {
  return date
    ? new Date(date).toISOString().replace("T", " ").replace("Z", " UTC")
    : "—";
}
export function bytes(value: string): number {
  return new TextEncoder().encode(value).length;
}
export function duration(start: string, end?: string): string {
  const seconds = Math.max(
    0,
    Math.floor(
      (new Date(end || Date.now()).getTime() - new Date(start).getTime()) /
        1000,
    ),
  );
  return [
    Math.floor(seconds / 3600),
    Math.floor((seconds % 3600) / 60),
    seconds % 60,
  ]
    .map((n) => String(n).padStart(2, "0"))
    .join(":");
}
export const waitingStates = new Set([
  "paused_gate",
  "paused_open_questions",
  "paused_user_stop",
  "awaiting_final_review",
]);
export const terminalStates = new Set(["done", "failed", "cancelled"]);
export function stateLabel(state: string): string {
  return (
    (
      {
        created: "Created",
        running: "Running",
        paused_gate: "Awaiting approval",
        paused_open_questions: "Questions",
        paused_user_stop: "Stopped",
        awaiting_final_review: "Final review",
        done: "Done",
        failed: "Failed",
        cancelled: "Cancelled",
      } as Record<string, string>
    )[state] || state
  );
}
export function stateTone(state: string): string {
  return waitingStates.has(state)
    ? "human"
    : state === "running"
      ? "work"
      : state === "done"
        ? "ok"
        : state === "failed"
          ? "fail"
          : "neutral";
}
export function stateIcon(state: string): string {
  return (
    (
      {
        created: "◌",
        running: "◔",
        paused_gate: "Ⅱ",
        paused_open_questions: "?",
        paused_user_stop: "□",
        awaiting_final_review: "◉",
        done: "✓",
        failed: "×",
        cancelled: "⊘",
      } as Record<string, string>
    )[state] || "•"
  );
}
export function problem(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}
export function short(value: string, length = 8) {
  return value.length > length ? value.slice(0, length) + "…" : value;
}
