import type { WorkspaceAction } from "./canter-api";
import type { WorkspaceOverview } from "./workspace-data";

const WRITE_TOOLS = new Set([
  "canter_draft_change",
  "canter_apply_change_under_policy",
  "canter_request_change_approval",
  "canter_upload_artifact",
  "canter_draft_initial_deployment",
  "canter_claim_task",
  "canter_finish_task",
]);

const attentionPhases = new Set(["drafted", "escalated"]);
const activePhases = new Set(["authorized", "queued", "running", "applying", "verifying", "compensating"]);
const completedPhases = new Set(["committed", "succeeded"]);
const declinedPhases = new Set(["rejected", "reverted"]);

export const activityStatuses = ["all", "attention", "active", "completed", "failed", "declined"] as const;
export type ActivityStatus = typeof activityStatuses[number];

export type ActivityOptions = { showReads: boolean; status: ActivityStatus; system: string };

export type TimelineEntry = {
  type: "item";
  kind: "Change" | "Deployment";
  id: string;
  system: string;
  summary: string;
  phase: string;
  href: string;
  at?: string;
  actor?: string;
  events: WorkspaceAction[];
};

export type TimelineEvent = { type: "event"; action: WorkspaceAction };
export type HistoryGroup = { label: string; key: string; items: (TimelineEntry | TimelineEvent)[] };

export type ActivityTimeline = {
  attention: TimelineEntry[];
  inProgress: TimelineEntry[];
  history: HistoryGroup[];
  systems: string[];
  counts: { total: number; shown: number };
};

export function effectivePhase(item: { phase: string; executionPhase?: string }) {
  return item.executionPhase || item.phase;
}

export function isReadAction(action: WorkspaceAction) {
  return action.action === "agent.tool" && !WRITE_TOOLS.has(action.metadata.tool ?? "");
}

function linkedItem(action: WorkspaceAction): { kind: "Change" | "Deployment"; id: string } | null {
  if (action.metadata.changeId) return { kind: "Change", id: action.metadata.changeId };
  if (action.metadata.deploymentId) return { kind: "Deployment", id: action.metadata.deploymentId };
  if (action.action.startsWith("change.")) return { kind: "Change", id: action.subject };
  if (action.action.startsWith("initial-deployment.")) return { kind: "Deployment", id: action.subject };
  return null;
}

function matchesStatus(phase: string, status: ActivityStatus) {
  if (status === "attention") return attentionPhases.has(phase);
  if (status === "active") return activePhases.has(phase);
  if (status === "completed") return completedPhases.has(phase);
  if (status === "failed") return phase === "failed";
  if (status === "declined") return declinedPhases.has(phase);
  return true;
}

function dayKey(value: string) {
  const date = new Date(value);
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}

function dayLabel(key: string, today: string, yesterday: string, currentYear: number) {
  if (key === today) return "Today";
  if (key === yesterday) return "Yesterday";
  const date = new Date(`${key}T12:00:00`);
  return date.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric", ...(date.getFullYear() === currentYear ? {} : { year: "numeric" }) });
}

function eventTime(entry: TimelineEntry | TimelineEvent) {
  return Date.parse(entry.type === "item" ? entry.at ?? "" : entry.action.occurredAt) || 0;
}

export function buildActivityTimeline(data: Pick<WorkspaceOverview, "changes" | "initialDeployments" | "actions">, opts: ActivityOptions): ActivityTimeline {
  const entries: TimelineEntry[] = [
    ...data.initialDeployments.map(item => ({
      type: "item" as const, kind: "Deployment" as const, id: item.id, system: item.system, summary: item.summary,
      phase: effectivePhase(item), href: `/app/changes/initial/${encodeURIComponent(item.id)}`, at: item.createdAt, events: [] as WorkspaceAction[],
    })),
    ...data.changes.map(item => ({
      type: "item" as const, kind: "Change" as const, id: item.id, system: item.system, summary: item.summary,
      phase: effectivePhase(item), href: `/app/changes/${encodeURIComponent(item.id)}?system=${encodeURIComponent(item.system)}`,
      at: item.createdAt, actor: item.draftedBy?.displayName || undefined, events: [] as WorkspaceAction[],
    })),
  ];
  const byItem = new Map(entries.map(entry => [`${entry.kind}:${entry.id}`, entry]));
  const standalone: WorkspaceAction[] = [];
  for (const action of data.actions) {
    const target = linkedItem(action);
    const entry = target && byItem.get(`${target.kind}:${target.id}`);
    if (entry) entry.events.push(action);
    else if (!isReadAction(action) || opts.showReads) standalone.push(action);
  }
  for (const entry of entries) {
    entry.events.sort((a, b) => (Date.parse(a.occurredAt) || 0) - (Date.parse(b.occurredAt) || 0));
    entry.actor ??= entry.events[0]?.actor.displayName || undefined;
    entry.at ??= entry.events[entry.events.length - 1]?.occurredAt;
  }
  const systems = [...new Set(entries.map(entry => entry.system))].sort();
  const filtered = entries.filter(entry => matchesStatus(entry.phase, opts.status) && (!opts.system || entry.system === opts.system));
  const shownEvents = opts.status !== "all" ? [] : standalone.filter(action => !opts.system || action.metadata.system === opts.system);
  const attention = filtered.filter(entry => attentionPhases.has(entry.phase)).sort((a, b) => eventTime(b) - eventTime(a));
  const inProgress = filtered.filter(entry => activePhases.has(entry.phase)).sort((a, b) => eventTime(b) - eventTime(a));
  const rest = filtered.filter(entry => !attentionPhases.has(entry.phase) && !activePhases.has(entry.phase));
  const historyItems: (TimelineEntry | TimelineEvent)[] = [
    ...rest.map(entry => entry as TimelineEntry),
    ...shownEvents.map(action => ({ type: "event" as const, action })),
  ].sort((a, b) => eventTime(b) - eventTime(a));
  const now = new Date();
  const today = dayKey(now.toISOString());
  const yesterday = dayKey(new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1, 12).toISOString());
  const groups = new Map<string, HistoryGroup>();
  for (const item of historyItems) {
    const at = item.type === "item" ? item.at : item.action.occurredAt;
    const key = at ? dayKey(at) : "unknown";
    let group = groups.get(key);
    if (!group) {
      group = { key, label: key === "unknown" ? "Earlier" : dayLabel(key, today, yesterday, now.getFullYear()), items: [] };
      groups.set(key, group);
    }
    group.items.push(item);
  }
  return {
    attention,
    inProgress,
    history: [...groups.values()],
    systems,
    counts: {
      total: entries.length + standalone.length,
      shown: attention.length + inProgress.length + historyItems.length,
    },
  };
}
