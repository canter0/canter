export type AuditActor = { kind: string; id: string; displayName: string; harness?: string };
export type AuditEvent = { id: string; action: string; subject: string; occurredAt: string; actor: AuditActor; metadata?: Record<string, unknown> };

const phrases: Record<string, string> = {
  "workspace.created": "created this organization",
  "workspace.renamed": "renamed this organization",
  "member.invited": "invited",
  "invite.revoked": "revoked invitation",
  "invite.accepted": "accepted an invitation",
  "invite.declined": "declined an invitation",
  "member.role-changed": "changed a role for",
  "member.removed": "removed member",
  "member.transferred": "transferred ownership to",
  "member.left": "left this organization",
  "agent.authorized": "connected agent",
  "agent.revoked": "revoked agent",
  "agent.permissions.updated": "changed permissions for agent",
  "agent.defaults.updated": "updated agent defaults",
  "agent.worker-connected": "connected a worker agent",
  "memory.created": "added organization note",
  "memory.updated": "edited organization note",
  "memory.deleted": "deleted organization note",
  "conversation.shared": "shared conversation",
  "conversation.unshared": "stopped sharing conversation",
  "system.registered": "registered system",
  "change.drafted": "drafted change",
  "change.authorized": "approved change",
  "change.approval-capability.created": "created approval for change",
  "change.approval-capability.consumed": "approved change execution",
  "change.approval-capability.failed": "failed approval for change",
  "change.policy-evaluated": "evaluated policy for change",
  "change.policy-authorized-and-queued": "policy-authorized and queued change",
  "execution.queued": "queued execution",
  "initial-deployment.drafted": "drafted deployment",
  "initial-deployment.authorized": "approved deployment",
  "initial-deployment.queued": "queued deployment",
  "artifact.uploaded": "uploaded artifact",
  "standing-policy.created": "created standing policy",
  "standing-policy.revoked": "revoked standing policy",
  "vps.drafted": "drafted VPS",
  "vps.approved": "approved VPS",
  "vps.deleted": "deleted VPS",
  "vps.deletion-requested": "requested VPS deletion",
  "sandbox.created": "created sandbox",
  "sandbox.slept": "slept sandbox",
  "sandbox.woke": "woke sandbox",
  "sandbox.deleted": "deleted sandbox",
  "sandbox.command": "ran a command in sandbox",
  "sandbox.force_stopped": "force-stopped sandbox",
};

export function auditActionPhrase(action: string) {
  if (action in phrases) return phrases[action];
  if (action.startsWith("agent.")) return action.slice(6).replaceAll(/[.-]/g, " ") + " agent";
  return action.replaceAll(/[.-]/g, " ");
}

export const securityNotificationActions = new Set([
  "agent.authorized",
  "agent.permissions.updated",
  "agent.revoked",
  "member.role-changed",
  "member.transferred",
  "member.removed",
  "sandbox.force_stopped",
]);
