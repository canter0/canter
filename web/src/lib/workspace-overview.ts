"use client";

import { useCallback, useEffect, useState } from "react";
import { usePathname, useRouter } from "next/navigation";
import { canterFetch, CanterAPIError, type ChangeSummary, type InitialDeploymentSummary, type Installation, type Me, type SystemRecord, type WorkspaceTask, type WorkspaceAction } from "./canter-api";

import type { Conversation } from "./operator-api";

type Overview = {
  conversations: Conversation[];
  agent: { available: boolean; model: string };
  workspace: Me["workspaces"][number];
  systems: SystemRecord[];
  installations: Installation[];
  changes: ChangeSummary[];
  initialDeployments: InitialDeploymentSummary[];
  tasks: WorkspaceTask[];
  actions: WorkspaceAction[];
};

export function useWorkspaceOverview() {
  const pathname = usePathname();
  const router = useRouter();
  const [data, setData] = useState<Overview | null>(null);
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    const options = { signal: controller.signal };
    let running = false;
    async function refresh() {
      if (running || controller.signal.aborted) return;
      running = true;
      try {
        const me = await canterFetch<Me>("/me", options);
        const workspace = me.workspaces[0];
        if (!workspace) throw new Error("No workspace is available.");
        const base = `/workspaces/${encodeURIComponent(workspace.id)}`;
        const [systems, installations, changes, deployments, tasks, actions, conversations] = await Promise.all([
          canterFetch<{ systems: SystemRecord[] }>(`${base}/systems`, options),
          canterFetch<{ installations: Installation[] }>(`/installations?workspaceId=${encodeURIComponent(workspace.id)}`, options),
          canterFetch<{ changes: ChangeSummary[] }>(`${base}/changes`, options),
          canterFetch<{ initialDeployments: InitialDeploymentSummary[] }>(`${base}/initial-deployments`, options),
          canterFetch<{ tasks: WorkspaceTask[] }>(`${base}/tasks`, options),
          canterFetch<{ actions: WorkspaceAction[] }>(`${base}/activity`, options),
          canterFetch<{ conversations: Conversation[]; agent: Overview["agent"] }>(`${base}/conversations`, options),
        ]);
        if (controller.signal.aborted) return;
        setData({ workspace, conversations: conversations.conversations ?? [], agent: conversations.agent, systems: systems.systems ?? [], installations: installations.installations ?? [], changes: changes.changes ?? [], initialDeployments: deployments.initialDeployments ?? [], tasks: tasks.tasks ?? [], actions: actions.actions ?? [] });
        setError("");
      } catch (cause) {
        if (cause instanceof CanterAPIError && cause.status === 401 && !controller.signal.aborted) { router.replace("/sign-in"); return; }
        if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Your workspace could not be loaded.");
      } finally {
        running = false;
      }
    }
    void refresh();
    const interval = window.setInterval(() => { if (document.visibilityState === "visible") void refresh(); }, 5000);
    const onFocus = () => { void refresh(); };
    window.addEventListener("focus", onFocus);
    return () => { controller.abort(); window.clearInterval(interval); window.removeEventListener("focus", onFocus); };
  }, [attempt, pathname, router]);

  const retry = useCallback(() => { setError(""); setAttempt(value => value + 1); }, []);
  return { data, error, loading: !data && !error, retry };
}

export type WorkspaceActivity = { id: string; summary: string; system: string; href: string; phase: string; createdAt?: string; kind: "Deployment" | "Change" };

export function workspaceActivities(data: Pick<Overview, "initialDeployments" | "changes">): WorkspaceActivity[] {
  return [
    ...data.initialDeployments.map(item => ({ ...item, kind: "Deployment" as const, href: `/app/changes/initial/${encodeURIComponent(item.id)}` })),
    ...data.changes.map(item => ({ ...item, kind: "Change" as const, href: `/app/changes/${encodeURIComponent(item.id)}?system=${encodeURIComponent(item.system)}` })),
  ].sort((a, b) => (Date.parse(b.createdAt ?? "") || 0) - (Date.parse(a.createdAt ?? "") || 0));
}

export function activityLabel(phase: string) {
  const labels: Record<string, string> = { drafted: "Needs review", authorized: "Approved", queued: "Queued", running: "Running", applying: "Applying", verifying: "Verifying", compensating: "Recovering", committed: "Completed", succeeded: "Completed", failed: "Failed", rejected: "Declined", reverted: "Reverted", escalated: "Needs attention" };
  return labels[phase] ?? phase;
}
