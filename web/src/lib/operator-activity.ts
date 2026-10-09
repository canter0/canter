import type { OperatorEvent } from "./operator-api";

export const toolLabels: Record<string, string> = {
  canter_search_web: "Searching the web", canter_open_web: "Opening a web source", canter_read_web: "Reading saved sources",
  canter_bash: "Working with workspace files", canter_save_context: "Saving project context", canter_search_history: "Finding earlier decisions", canter_read_history: "Reading conversation context", canter_read_result: "Reading saved results", canter_create_task: "Queuing agent task", canter_list_tasks: "Reading tasks", canter_inspect_task: "Checking task progress", canter_read_task_context: "Reading task context",
  canter_prepare_vps: "Preparing VPS review", canter_list_vps: "Reading your servers", canter_inspect_vps: "Checking VPS status", canter_show_compute: "Preparing compute plan", canter_estimate_compute_cost: "Calculating Canter compute estimate", canter_show_storage: "Checking storage capabilities",
  canter_show_repositories: "Opening GitHub repositories", canter_show_repository_changes: "Reading code changes",
  canter_show_apps: "Reading apps", canter_show_deployments: "Reading deployments", canter_show_billing: "Reading billing", canter_show_activity: "Reading activity", canter_show_agents: "Reading agent access",
  canter_inspect_repository: "Inspecting repository", canter_read_repository_file: "Reading source", canter_prepare_repository_deployment: "Preparing deployment", canter_capabilities: "Checking capabilities",
  canter_inspect_system: "Inspecting app", canter_inspect_initial_deployment: "Reading deployment", canter_inspect_initial_deployment_execution: "Reading execution", canter_inspect_change: "Reading change", canter_draft_change: "Preparing change", canter_list_changes: "Reading changes", canter_list_standing_policies: "Checking policies", canter_apply_change_under_policy: "Applying authorized change",
};

export function operatorToolLabel(name: unknown) {
  const key = typeof name === "string" ? name : "";
  return toolLabels[key] ?? (key ? key.replace(/^canter_/, "").replaceAll("_", " ") : "Running an action");
}

export function operatorActivity(events: OperatorEvent[], running: boolean, hasAnswer = false) {
  const tools = new Map<string, OperatorEvent>();
  for (const event of events) {
    if (event.kind === "tool") tools.set(String(event.data.callId ?? event.sequence), event);
  }
  const actions = [...tools.values()];
  const count = actions.length;
  const failed = actions.filter(event => event.data.status === "failed").length;
  if (!running) return { count, failed, label: `${count} ${count === 1 ? "action" : "actions"}${failed ? ` · ${failed} failed` : ""}` };
  const active = actions.findLast(event => event.data.status === "running");
  const lastAction = actions.at(-1);
  const lastText = events.findLast(event => event.kind === "text");
  const label = active ? operatorToolLabel(active.data.name)
    : hasAnswer || (lastText && (!lastAction || lastText.sequence > lastAction.sequence)) ? "Writing response"
    : "Thinking";
  return { count, failed, label };
}
