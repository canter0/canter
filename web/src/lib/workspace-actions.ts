import type { WorkspaceAction } from "./canter-api";

const tools: Record<string, string> = {
  canter_whoami: "Checked connection",
  canter_bootstrap: "Read workspace",
  canter_list_changes: "Listed changes",
  canter_inspect_system: "Inspected app",
  canter_draft_change: "Prepared change",
  canter_inspect_change: "Inspected change",
  canter_inspect_change_execution: "Checked execution",
  canter_list_standing_policies: "Checked policies",
  canter_apply_change_under_policy: "Requested change under policy",
  canter_request_change_approval: "Requested review",
  canter_upload_artifact: "Uploaded app bundle",
  canter_draft_initial_deployment: "Prepared deployment",
  canter_list_initial_deployments: "Listed deployments",
  canter_inspect_initial_deployment: "Inspected deployment",
  canter_inspect_initial_deployment_execution: "Checked deployment execution",
  canter_list_tasks: "Listed tasks",
  canter_inspect_task: "Read task",
  canter_read_task_context: "Read task context",
  canter_claim_task: "Picked up task",
  canter_finish_task: "Reported task result",
};
const actions: Record<string, string> = {
  "agent.worker-connected": "Connected a worker", "agent.authorized": "Agent connected", "agent.revoked": "Agent disconnected",
  "task.created": "Task created", "task.working": "Task started", "task.completed": "Task reported complete", "task.failed": "Task reported failed",
  "system.registered": "App registered", "artifact.uploaded": "App bundle uploaded",
  "change.drafted": "Change prepared", "change.authorized": "Change approved", "execution.queued": "Execution queued",
  "initial-deployment.drafted": "Deployment prepared", "initial-deployment.authorized": "Deployment approved", "initial-deployment.queued": "Deployment queued",
  "standing-policy.created": "Policy created", "standing-policy.revoked": "Policy revoked",
  "change.approval-capability.created": "Review requested", "change.approval-capability.consumed": "Review approved",
  "change.policy-evaluated": "Policy evaluated", "change.policy-authorized-and-queued": "Change approved by policy",
};
export function actionTitle(action: WorkspaceAction) {
  return action.action === "agent.tool" ? tools[action.metadata.tool ?? ""] ?? "Agent action" : actions[action.action] ?? action.action.replace(/[.-]/g, " ");
}
export function actionHref(action: WorkspaceAction): string | null {
  if (action.metadata.taskId) return `/app/tasks/${encodeURIComponent(action.metadata.taskId)}`;
  if (action.action.startsWith("task.")) return `/app/tasks/${encodeURIComponent(action.subject)}`;
  if (action.action === "initial-deployment.drafted" || action.action === "initial-deployment.authorized") return `/app/changes/initial/${encodeURIComponent(action.subject)}`;
  if (action.metadata.deploymentId) return `/app/changes/initial/${encodeURIComponent(action.metadata.deploymentId)}`;
  if (action.action.startsWith("change.") && action.metadata.system) return `/app/changes/${encodeURIComponent(action.metadata.changeId ?? action.subject)}?system=${encodeURIComponent(action.metadata.system)}`;
  if (action.action === "system.registered") return `/app/system/${encodeURIComponent(action.subject)}`;
  if (action.actor.kind === "agent") return `/app/agents/${encodeURIComponent(action.actor.id)}`;
  return null;
}
