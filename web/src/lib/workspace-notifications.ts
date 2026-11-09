import type { WorkspaceOverview } from "./workspace-data";

export type WorkspaceNotification = { id: string; title: string; detail: string; href: string; kind: "review" | "failure" | "invite" | "security"; updatedAt: string; inviteId?: string; workspaceId?: string };
export type NotificationReceipts = Record<string, true>;

export function notificationReceiptKey(account: string, workspace: string) {
  return `canter:notification-receipts:${encodeURIComponent(account)}:${encodeURIComponent(workspace)}`;
}

export function parseNotificationReceipts(raw: string): NotificationReceipts {
  if (!raw || raw.length > 100_000) return {};
  try {
    const value: unknown = JSON.parse(raw);
    if (!value || typeof value !== "object" || Array.isArray(value)) return {};
    return Object.fromEntries(Object.entries(value).filter(([key, read]) => key.length < 500 && read === true).slice(-400)) as NotificationReceipts;
  } catch { return {}; }
}

/** Attention comes from actual workspace state, never from decorative fixture alerts. */
export function workspaceNotifications(data: WorkspaceOverview): WorkspaceNotification[] {
  const items: WorkspaceNotification[] = [];
  for (const invite of data.invites ?? []) {
    items.push({ id: `invite:${invite.id}`, title: `${invite.inviterEmail || "Someone"} invited you to ${(invite.workspaceName === "default" ? "Personal" : invite.workspaceName) || "an organization"}`, detail: `Join as ${invite.role}.`, href: "", kind: "invite", updatedAt: invite.expiresAt, inviteId: invite.id, workspaceId: invite.workspaceId });
  }
  for (const conversation of data.conversations) {
    if (conversation.workspaceId !== data.workspace.id || conversation.status !== "failed") continue;
    items.push({ id: `conversation:${conversation.id}:failed:${conversation.updatedAt}`, title: conversation.title, detail: "This session needs your attention.", href: `/app/conversations/${encodeURIComponent(conversation.id)}`, kind: "failure", updatedAt: conversation.updatedAt });
  }
  for (const task of data.tasks) {
    if (task.workspaceId !== data.workspace.id || task.status !== "failed") continue;
    items.push({ id: `task:${task.id}:failed:${task.updatedAt}`, title: task.prompt, detail: "An agent task could not complete.", href: `/app/tasks/${encodeURIComponent(task.id)}`, kind: "failure", updatedAt: task.updatedAt });
  }
  for (const change of data.changes) {
    const phase = ["failed", "escalated"].includes(change.executionPhase ?? "") ? change.executionPhase : change.phase;
    if (!["drafted", "failed", "escalated"].includes(phase ?? "")) continue;
    const review = phase === "drafted";
    items.push({ id: `change:${change.id}:${phase}:${change.digest}`, title: change.summary, detail: review ? `${change.system} · A change is ready for your review.` : `${change.system} · A change needs your attention.`, href: `/app/changes/${encodeURIComponent(change.id)}?system=${encodeURIComponent(change.system)}`, kind: review ? "review" : "failure", updatedAt: change.createdAt ?? "" });
  }
  for (const deployment of data.initialDeployments) {
    if (!["drafted", "failed"].includes(deployment.phase)) continue;
    const review = deployment.phase === "drafted";
    items.push({ id: `deployment:${deployment.id}:${deployment.phase}:${deployment.digest}`, title: deployment.summary, detail: review ? `${deployment.system} · A deployment is ready for your review.` : `${deployment.system} · A deployment could not complete.`, href: `/app/changes/initial/${encodeURIComponent(deployment.id)}`, kind: review ? "review" : "failure", updatedAt: deployment.createdAt ?? "" });
  }
  return [...new Map(items.map(item => [item.id, item])).values()].sort((a, b) => (Date.parse(b.updatedAt) || 0) - (Date.parse(a.updatedAt) || 0) || a.id.localeCompare(b.id));
}

type SecurityEvent = { id: string; action: string; subject: string; occurredAt: string; actor: { kind: string; id: string; displayName: string; harness?: string } };
const securityActions = new Set(["agent.authorized", "agent.permissions.updated", "agent.revoked", "member.role-changed", "member.transferred", "member.removed"]);
const securityVerb: Record<string, string> = { "agent.authorized": "connected", "agent.permissions.updated": "changed permissions for", "agent.revoked": "revoked", "member.role-changed": "changed a role for", "member.transferred": "transferred ownership to", "member.removed": "removed" };
// Owners and operators watch agent and membership changes made by other people.
export function securityNotifications(events: SecurityEvent[] | undefined, viewerId: string): WorkspaceNotification[] {
  return (events ?? []).filter(event => securityActions.has(event.action) && event.actor.id !== viewerId).map(event => ({
    id: `security:${event.id}`,
    title: `${event.actor.displayName || event.actor.id} ${securityVerb[event.action]} ${event.subject}`,
    detail: event.actor.kind === "agent" ? "Performed by a connected agent." : "",
    href: "/app/settings/audit",
    kind: "security",
    updatedAt: event.occurredAt,
  }));
}
