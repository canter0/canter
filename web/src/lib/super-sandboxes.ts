export type SuperSandboxState = "queued" | "running" | "cancelling" | "succeeded" | "failed" | "cancelled" | "needs_attention";
export type SuperSandboxTaskState = "queued" | "starting" | "running" | "unknown" | "succeeded" | "failed" | "cancelled" | "outcome_unavailable";
export type SuperSandboxTask = {
  id: string; name: string; index: number; command: string; directory?: string;
  timeoutSeconds: number; state: SuperSandboxTaskState; workerId?: string; jobId?: string;
  startedAt?: string; finishedAt?: string; exitCode?: number; error?: string; reason?: string;
};
export type SuperSandbox = {
  id: string; workspaceId: string; name: string; state: SuperSandboxState;
  workerIds: string[]; maxConcurrency: number; maxWorkerSeconds: number; reservedWorkerSeconds: number;
  createdAt: string; startedAt?: string; finishedAt?: string; failure?: string; blockedReason?: string; tasks: SuperSandboxTask[];
};
export type SuperSandboxListResponse = { superSandboxes: SuperSandbox[]; enabled: boolean };
export type SuperSandboxSummary = Pick<SuperSandbox, "id" | "name" | "state" | "blockedReason" | "createdAt" | "startedAt" | "finishedAt" | "maxWorkerSeconds" | "reservedWorkerSeconds" | "maxConcurrency"> & {
  workerCount: number;
  progress: { total: number; settled: number; succeeded: number; unknown: number; outcomeUnavailable: number };
};
export type SuperSandboxSummaryResponse = { superSandboxes: SuperSandboxSummary[]; enabled: boolean; nextCursor: string };

export function superSandboxTerminal(state: string): boolean {
  // needs_attention is unresolved, not a completed run. Cancellation must
  // remain available when a worker's outcome is unknown.
  return state === "succeeded" || state === "failed" || state === "cancelled";
}

export function superSandboxStateLabel(state: SuperSandboxState | SuperSandboxTaskState): string {
  if (state === "needs_attention") return "Needs attention";
  if (state === "unknown") return "Outcome unknown · needs attention";
  if (state === "outcome_unavailable") return "Outcome unavailable";
  return state.charAt(0).toUpperCase() + state.slice(1);
}

const blockerLabels: Record<string, string> = {
  worker_unavailable: "Selected workers are sleeping or not ready. Wake the worker to continue; the batch does not replace it.",
  worker_replaced: "A selected worker was removed or replaced. Its commands will not be reassigned to a new machine.",
  workers_busy: "Selected workers are currently reserved by other tasks.",
  missing_exit_code: "A worker reported a result without an exit code. The task outcome is unresolved.",
  worker_interrupted: "A worker reported an interrupted job. Whether the command finished is unresolved.",
  receipt_unconfirmed: "A worker's job receipt could not be confirmed. The task outcome is unresolved.",
  worker_unobservable: "A worker could not be observed. The task outcome is unresolved.",
  generation_changed: "A worker changed generation. The task outcome is unresolved.",
  worker_removed: "A worker was removed before its job outcome was recorded.",
  start_rejected: "The worker rejected a command before launch; its job is being closed.",
  outcome_unknown: "A task outcome is unknown. The worker stays reserved until it is resolved.",
};

export function superSandboxBlockerLabel(reason?: string): string {
  if (!reason) return "";
  return blockerLabels[reason] ?? "This batch needs attention. Inspect its tasks before deciding what to do next.";
}

export function superSandboxProgress(tasks: SuperSandboxTask[]) {
  const settled = tasks.filter(task => superSandboxTerminal(task.state) || task.state === "outcome_unavailable").length;
  const succeeded = tasks.filter(task => task.state === "succeeded").length;
  const unknown = tasks.filter(task => task.state === "unknown").length;
  const outcomeUnavailable = tasks.filter(task => task.state === "outcome_unavailable").length;
  return { settled, succeeded, unknown, outcomeUnavailable, total: tasks.length };
}

export function workerDuration(seconds: number): string {
  const value = Math.max(0, Math.ceil(seconds));
  const hours = Math.floor(value / 3600), minutes = Math.floor(value % 3600 / 60), rest = value % 60;
  return [hours ? `${hours}h` : "", minutes ? `${minutes}m` : "", rest || !value ? `${rest}s` : ""].filter(Boolean).join(" ");
}

export function runtimeAllocation(run: Pick<SuperSandbox, "reservedWorkerSeconds" | "maxWorkerSeconds">): string {
  return `${workerDuration(run.reservedWorkerSeconds)} / ${workerDuration(run.maxWorkerSeconds)}`;
}

export function superSandboxRequestPrompt(workspaceId: string, workspaceName: string, id?: string): string {
  const scope = `organisation ${JSON.stringify(workspaceName)} (ID ${workspaceId})`;
  if (id) return `I want to request cancellation of Super Sandbox ${id} in ${scope}. Inspect its current state, including in-flight or unknown tasks, and explain the consequences before proceeding. This is a request for review, not automatic execution. Do not delete, sleep or replace its workers.`;
  return `Help me plan a Super Sandbox in ${scope}. Select existing ready workers and partition the work into explicit tasks with commands, directories and timeouts. Agree on concurrency and a runtime budget: reserve the worst-case sum of task timeouts upfront within maxWorkerSeconds. Explain that worker VM billing continues separately, files stay local to each worker, and RAM is not pooled. Do not create, delete or sleep workers. Confirm the plan before executing.`;
}
