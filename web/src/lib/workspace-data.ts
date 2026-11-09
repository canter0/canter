import type { ChangeSummary, InitialDeploymentSummary, Installation, Me, Product, SystemRecord, WorkspaceInvitation, WorkspaceTask, WorkspaceAction } from "./canter-api";
import type { Conversation } from "./operator-api";

export type WorkspaceOverview = {
  account: Me["account"];
  workspaces?: Me["workspaces"];
  workspace: Me["workspaces"][number];
  conversations: Conversation[];
  agent: { available: boolean; model: string };
  systems: SystemRecord[];
  installations: Installation[];
  changes: ChangeSummary[];
  initialDeployments: InitialDeploymentSummary[];
  tasks: WorkspaceTask[];
  actions: WorkspaceAction[];
  invites?: WorkspaceInvitation[];
  products?: Product[];
};

export const workspaceResources = ["conversations", "systems", "installations", "changes", "initialDeployments", "tasks", "actions", "invites", "products"] as const;
export type WorkspaceResource = typeof workspaceResources[number];
export type WorkspaceBootstrap = { data: WorkspaceOverview; conversationsLoaded: boolean };

export function workspaceResourcePath(workspace: string, resource: WorkspaceResource) {
  const base = `/workspaces/${encodeURIComponent(workspace)}`;
  if (resource === "installations") return `/installations?workspaceId=${encodeURIComponent(workspace)}`;
  if (resource === "invites") return "/me/invites";
  return `${base}/${resource === "initialDeployments" ? "initial-deployments" : resource === "actions" ? "activity" : resource}`;
}

// Only a screen's own data can hold up its first render. Other lists warm in parallel.
export function requiredWorkspaceResources(pathname: string): WorkspaceResource[] {
  if (pathname === "/app/system") return ["systems"];
  if (pathname.startsWith("/app/agents") || pathname === "/app/settings/agent") return ["installations"];
  if (pathname === "/app/changes") return ["actions", "changes", "initialDeployments"];
  if (pathname.startsWith("/app/tasks/")) return ["tasks", "actions", "installations"];
  if (pathname === "/app" || pathname.startsWith("/app/wrens") || pathname.startsWith("/app/conversations/")) return ["conversations"];
  return [];
}
