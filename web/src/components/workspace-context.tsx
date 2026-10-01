"use client";

import { createContext, useCallback, useContext, useEffect, useRef, useState, useSyncExternalStore, type ReactNode } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { PrefetchKind } from "next/dist/client/components/router-reducer/router-reducer-types";
import { AgentConnectionDialog } from "./agent-connection-dialog";
import { useWorkspaceOverview } from "@/lib/workspace-overview";
import type { WorkspaceBootstrap } from "@/lib/workspace-data";
import { createConversationCache, type ConversationCache } from "@/lib/conversation-cache";
import { conversationDetail } from "@/lib/operator-api";

const WorkspaceContext = createContext<(ReturnType<typeof useWorkspaceOverview> & { conversationCache: ConversationCache; prefetchConversation: (id: string) => void; collapsed: boolean; setCollapsed: (value: boolean) => void; setPageTitle: (value: string) => void; connectAgent: () => void }) | null>(null);
const sidebarKey = "canter:sidebar-collapsed";
const subscribeSidebar = (onChange: () => void) => {
  window.addEventListener("storage", onChange);
  window.addEventListener("canter-sidebar", onChange);
  return () => { window.removeEventListener("storage", onChange); window.removeEventListener("canter-sidebar", onChange); };
};

export function WorkspaceProvider({ children, initial }: { children: ReactNode; initial: WorkspaceBootstrap }) {
  const overview = useWorkspaceOverview(initial);
  const router = useRouter();
  const [conversationCache] = useState(() => createConversationCache(initial.data.workspace.id, id => conversationDetail(initial.data.workspace.id, id)));
  const prefetched = useRef(new Set<string>());
  const prefetchConversation = useCallback((id: string) => {
    if (prefetched.current.has(id)) return;
    prefetched.current.add(id);
    if (prefetched.current.size > 24) prefetched.current.delete(prefetched.current.values().next().value!);
    // Manual prefetch includes the server-rendered messages, not just the shell.
    router.prefetch(`/app/conversations/${encodeURIComponent(id)}`, { kind: PrefetchKind.FULL, onInvalidate: () => { prefetched.current.delete(id); } });
  }, [router]);
  const pathname = usePathname();
  const recentIds = overview.data.conversations.slice(0, 3).map(item => item.id).join(",");
  useEffect(() => {
    const connection = (navigator as Navigator & { connection?: { saveData?: boolean; effectiveType?: string } }).connection;
    if (connection?.saveData || ["slow-2g", "2g"].includes(connection?.effectiveType ?? "")) return;
    const timers = recentIds.split(",").filter(id => id && pathname !== `/app/conversations/${encodeURIComponent(id)}`).map((id, index) => window.setTimeout(() => prefetchConversation(id), 250 + index * 200));
    return () => timers.forEach(timer => window.clearTimeout(timer));
  }, [recentIds, pathname, prefetchConversation]);
  const searchParams = useSearchParams();
  const routeKey = `${pathname}?${searchParams.toString()}`;
  const [viewTitle, setViewTitle] = useState({ route: "", title: "" });
  const setPageTitle = useCallback((title: string) => setViewTitle(current => current.route === routeKey && current.title === title ? current : { route: routeKey, title }), [routeKey]);
  // Update during the route render so Next's announcer never reads the previous
  // screen's title while the destination shell is still mounting.
  const conversationTitle = overview.data?.conversations.find(item => pathname === `/app/conversations/${encodeURIComponent(item.id)}`)?.title;
  const routeTitle = conversationTitle ?? (pathname === "/app" ? "New conversation"
    : pathname === "/app/account" ? "Profile settings"
    : pathname.startsWith("/app/account/connections") ? "Connections settings"
    : pathname.startsWith("/app/settings/secrets") ? "Secrets settings"
    : pathname.startsWith("/app/settings/agent") ? "Agents settings"
    : pathname.startsWith("/app/settings") ? "General settings"
    : pathname.startsWith("/app/billing") ? `${searchParams.get("view") === "plans" ? "Plans" : searchParams.get("view") === "invoices" ? "Invoices" : "Usage"} settings`
    : pathname.endsWith("/policies") ? "Standing policies"
    : pathname.startsWith("/app/system/") ? "App details"
    : pathname.startsWith("/app/system") ? "Apps"
    : pathname.startsWith("/app/agents") ? "Agents"
    : pathname.startsWith("/app/changes/") ? "Deployment review"
    : pathname.startsWith("/app/changes") ? "Activity"
    : "Workspace");
  const pageTitle = viewTitle.route === routeKey ? viewTitle.title : routeTitle;
  const [fallbackCollapsed, setFallbackCollapsed] = useState(false);
  const collapsed = useSyncExternalStore(subscribeSidebar, () => { try { return localStorage.getItem(sidebarKey) === "true"; } catch { return fallbackCollapsed; } }, () => false);
  function setCollapsed(value: boolean) {
    setFallbackCollapsed(value);
    try { localStorage.setItem(sidebarKey, String(value)); window.dispatchEvent(new Event("canter-sidebar")); } catch { /* The sidebar remains usable without storage. */ }
  }
  const [connectionOpen, setConnectionOpen] = useState(false);
  return <WorkspaceContext.Provider value={{ ...overview, conversationCache, prefetchConversation, collapsed, setCollapsed, setPageTitle, connectAgent: () => setConnectionOpen(true) }}><title>{`${pageTitle} — Canter`}</title>{children}{connectionOpen && overview.data ? <AgentConnectionDialog workspaceId={overview.data.workspace.id} defaultAuthority={overview.data.workspace.agentAuthority} onClose={() => setConnectionOpen(false)} onConnected={overview.retry} /> : null}</WorkspaceContext.Provider>;
}

export function useWorkspace() {
  const value = useContext(WorkspaceContext);
  if (!value) throw new Error("WorkspaceProvider is required.");
  return value;
}
