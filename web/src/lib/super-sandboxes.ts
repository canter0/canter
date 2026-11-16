export type SuperSandboxState = "queued" | "running" | "cancelling" | "succeeded" | "failed" | "cancelled" | "needs_attention";
export type SuperSandboxTaskState = "queued" | "starting" | "running" | "unknown" | "succeeded" | "failed" | "cancelled";
export type SuperSandboxTask = {
  id: string; name: string; index: number; command: string; directory?: string;
  timeoutSeconds: number; state: SuperSandboxTaskState; workerId?: string; jobId?: string;
  startedAt?: string; finishedAt?: string; exitCode?: number; error?: string;
};
export type SuperSandbox = {
  id: string; workspaceId: string; name: string; state: SuperSandboxState;
  workerIds: string[]; maxConcurrency: number; maxWorkerSeconds: number; reservedWorkerSeconds: number;
  createdAt: string; startedAt?: string; finishedAt?: string; failure?: string; tasks: SuperSandboxTask[];
};
export type SuperSandboxListResponse = { superSandboxes: SuperSandbox[]; enabled: boolean };

export function superSandboxTerminal(state: string): boolean {
  // needs_attention is unresolved, not a completed run. Cancellation must
  // remain available when a worker's outcome is unknown.
  return state === "succeeded" || state === "failed" || state === "cancelled";
}

export function superSandboxStateLabel(state: SuperSandboxState | SuperSandboxTaskState): string {
  if (state === "needs_attention") return "Needs attention";
  if (state === "unknown") return "Unknown · needs attention";
  return state.charAt(0).toUpperCase() + state.slice(1);
}

export function superSandboxProgress(tasks: SuperSandboxTask[]) {
  const completed = tasks.filter(task => superSandboxTerminal(task.state)).length;
  const succeeded = tasks.filter(task => task.state === "succeeded").length;
  const unknown = tasks.filter(task => task.state === "unknown").length;
  return { completed, succeeded, unknown, total: tasks.length };
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
