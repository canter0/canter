"use client";

import { useCallback, useEffect, useState } from "react";
import { usePathname } from "next/navigation";
import { canterFetch, CanterAPIError, type Me } from "./canter-api";
import { requiredWorkspaceResources, workspaceResources, workspaceResourcePath, type WorkspaceBootstrap, type WorkspaceOverview as Overview, type WorkspaceResource } from "./workspace-data";

type ResourceState = { loaded: boolean; error: string };

export function useWorkspaceOverview(initial: WorkspaceBootstrap) {
  const pathname = usePathname();
  const [data, setData] = useState(initial.data);
  const [resources, setResources] = useState(() => Object.fromEntries(workspaceResources.map(resource => [resource, { loaded: resource === "conversations" && initial.conversationsLoaded, error: "" }])) as Record<WorkspaceResource, ResourceState>);
  const [attempt, setAttempt] = useState(0);
  const workspaceId = initial.data.workspace.id;
  const accountId = initial.data.account.id;

  useEffect(() => {
    const controller = new AbortController();
    const options = { signal: controller.signal };
    const running = new Set<string>();
    const handleAuth = (cause: unknown) => {
      if (cause instanceof CanterAPIError && cause.status === 401) {
        // A full navigation disposes of every private in-memory snapshot.
        window.location.replace("/sign-in?next=/app");
      } else if(cause instanceof CanterAPIError&&cause.status===403){void refreshAccount();}
    };
    async function refreshResource(resource: WorkspaceResource) {
      if (running.has(resource) || controller.signal.aborted) return;
      running.add(resource);
      try {
        const result = await canterFetch<Partial<Overview>>(workspaceResourcePath(workspaceId, resource), options);
        if (controller.signal.aborted) return;
        setData(current => ({ ...current, [resource]: result[resource] ?? [], ...(resource === "conversations" && result.agent ? { agent: result.agent } : {}) }));
        setResources(current => ({ ...current, [resource]: { loaded: true, error: "" } }));
      } catch (cause) {
        if (!controller.signal.aborted) {
          handleAuth(cause);
          setResources(current => ({ ...current, [resource]: { ...current[resource], error: cause instanceof Error ? cause.message : "This view could not be loaded." } }));
        }
      } finally { running.delete(resource); }
    }
    async function refreshAccount() {
      if (running.has("me") || controller.signal.aborted) return;
      running.add("me");
      try {
        const me = await canterFetch<Me>("/me", options);
        if (controller.signal.aborted) return;
        const selected=me.workspaces.find(workspace=>workspace.id===workspaceId);
        if (me.account.id !== accountId || !selected) { window.location.replace("/app/wrens"); return; }
        setData(current => ({ ...current, account: me.account, workspace: selected,workspaces:me.workspaces }));
      } catch (cause) { if (!controller.signal.aborted) handleAuth(cause); }
      finally { running.delete("me"); }
    }
    function refresh() {
      void refreshAccount();
      for (const resource of workspaceResources) void refreshResource(resource);
    }
    // The initial account and conversation list arrived with the HTML. Every
    // other resource loads independently; route changes never restart this work.
    for (const resource of workspaceResources) {
      if (resource !== "conversations" || !initial.conversationsLoaded || attempt > 0) void refreshResource(resource);
    }
    if (attempt > 0) void refreshAccount();
    const refreshVisible = () => { if (document.visibilityState === "visible") refresh(); };
    const interval = window.setInterval(refreshVisible, 5000);
    window.addEventListener("focus", refresh);
    document.addEventListener("visibilitychange", refreshVisible);
    const switched=(event:StorageEvent)=>{if(event.key!=="canter-workspace-change"||!event.newValue)return;try{const selected=JSON.parse(event.newValue);if(selected.accountId===accountId&&typeof selected.workspaceId==="string"&&selected.workspaceId!==workspaceId)window.location.replace("/app/wrens");}catch{/* Ignore unrelated/malformed storage values. */}};
    window.addEventListener("storage",switched);
    return () => { controller.abort(); window.clearInterval(interval); window.removeEventListener("focus", refresh); document.removeEventListener("visibilitychange", refreshVisible);window.removeEventListener("storage",switched); };
  }, [attempt, workspaceId, accountId, initial.conversationsLoaded]);

  const required = requiredWorkspaceResources(pathname);
  const unavailable = required.some(resource => !resources[resource].loaded);
  const error = required.map(resource => resources[resource].error).filter(Boolean).join(" ");
  const retry = useCallback(() => setAttempt(value => value + 1), []);
  return { data, error, loading: unavailable && !error, unavailable, resources, retry };
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
