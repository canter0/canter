"use client";

import { createContext, useCallback, useContext, useEffect, useRef, useState, useSyncExternalStore, type ReactNode } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { PrefetchKind } from "next/dist/client/components/router-reducer/router-reducer-types";
import { AgentConnectionDialog } from "./agent-connection-dialog";
import { useWorkspaceOverview } from "@/lib/workspace-overview";
import type { WorkspaceBootstrap } from "@/lib/workspace-data";
import { createConversationCache, type ConversationCache } from "@/lib/conversation-cache";
import { conversationDetail } from "@/lib/operator-api";
import { useReleaseUpdate } from "./use-release-update";
import { appendSessionTab, canTravelSessionTab, closeSessionTab, createSessionTabs, currentSessionEntry, openSessionTab, readSessionTabs, sanitizeSessionHref, selectSessionTab, travelSessionTab, travelSessionTabTo, visitSessionRoute, writeSessionTabs, type SessionTabsState } from "@/lib/session-tabs";
import { SessionTabsContext, type SessionTabsController } from "./session-tabs-context";
export { useSessionTabs } from "./session-tabs-context";

const WorkspaceContext = createContext<(ReturnType<typeof useWorkspaceOverview> & { releaseUpdate: ReturnType<typeof useReleaseUpdate>; conversationCache: ConversationCache; prefetchConversation: (id: string) => void; collapsed: boolean; setCollapsed: (value: boolean) => void; setPageTitle: (value: string) => void; connectAgent: () => void }) | null>(null);
const sidebarKey = "canter:sidebar-collapsed";
const subscribeSidebar = (onChange: () => void) => {
  window.addEventListener("storage", onChange);
  window.addEventListener("canter-sidebar", onChange);
  return () => { window.removeEventListener("storage", onChange); window.removeEventListener("canter-sidebar", onChange); };
};

export function WorkspaceProvider({ children, initial }: { children: ReactNode; initial: WorkspaceBootstrap }) {
  const overview = useWorkspaceOverview(initial);
  const releaseUpdate = useReleaseUpdate();
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
    : pathname === "/app/customize" ? "Customize"
    : pathname === "/app/account" ? "Profile settings"
    : pathname.startsWith("/app/account/connections") ? "Connections settings"
    : pathname.startsWith("/app/settings/secrets") ? "Secrets settings"
    : pathname.startsWith("/app/settings/people") ? "People settings"
    : pathname.startsWith("/app/settings/memory") ? "Memory settings"
    : pathname.startsWith("/app/settings/audit") ? "Audit log settings"
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

  // Session tab controller: browser-style per-tab history, persisted per
  // account/workspace in sessionStorage. State starts null so the server render
  // and first client render match; the strip appears after the client restore.
  const tabScope = `${initial.data.account.id}:${initial.data.workspace.id}`;
  const tabsRef = useRef<SessionTabsState | null>(null);
  const [tabsState, setTabsState] = useState<SessionTabsState | null>(null);
  const pendingNavigation = useRef<{ from: string; to: string } | null>(null);
  const pendingTravel = useRef<string | null>(null);
  const tabSequence = useRef(0);
  const newTabId = useCallback(() => `tab-${Date.now().toString(36)}-${(tabSequence.current++).toString(36)}`, []);
  const applyTabs = useCallback((next: SessionTabsState) => {
    tabsRef.current = next;
    setTabsState(next);
    writeSessionTabs(tabScope, next);
  }, [tabScope]);
  const query = searchParams.toString();
  const currentHref = sanitizeSessionHref(`${pathname}${query ? `?${query}` : ""}`) ?? "/app";
  const currentHrefRef = useRef(currentHref);
  useEffect(() => { currentHrefRef.current = currentHref; }, [currentHref]);
  const tabRouteTitle = pathname === "/app" ? "Home" : pageTitle;
  const titleForHref = useCallback((href: string) => {
    const path = href.split(/[?#]/, 1)[0];
    if (path === "/app") return "Home";
    const conversation = overview.data.conversations.find(item => path === `/app/conversations/${encodeURIComponent(item.id)}`);
    return conversation?.title ?? "Workspace";
  }, [overview.data.conversations]);
  useEffect(() => {
    if (tabsRef.current) return;
    applyTabs(readSessionTabs(tabScope) ?? createSessionTabs("initial"));
  }, [applyTabs, tabScope, newTabId]);
  // Genuine browser traversal (the app's own replace/push does not fire
  // popstate) moves inside the active tab's stack rather than pushing.
  useEffect(() => {
    const pop = () => {
      pendingTravel.current = sanitizeSessionHref(`${window.location.pathname}${window.location.search}${window.location.hash}`);
    };
    window.addEventListener("popstate", pop);
    return () => window.removeEventListener("popstate", pop);
  }, []);
  useEffect(() => {
    const state = tabsRef.current;
    if (!state) return;
    if (pendingTravel.current) {
      const target = pendingTravel.current;
      pendingTravel.current = null;
      if (target === currentHref) {
        const next = travelSessionTabTo(state, target);
        if (next !== state) { applyTabs(next); return; }
      }
    }
    // While a tab switch/close/travel navigation is in flight the previous
    // route is stale. Canonical redirects land elsewhere and must still record.
    const pending = pendingNavigation.current;
    if (pending) {
      if (currentHref === pending.from && currentHref !== pending.to) return;
      pendingNavigation.current = null;
    }
    const next = visitSessionRoute(state, currentHref, tabRouteTitle);
    if (next !== state) applyTabs(next);
  }, [currentHref, tabRouteTitle, applyTabs]);
  const navigateTab = useCallback((href: string) => {
    const pending = pendingNavigation.current;
    if (href === currentHrefRef.current) {
      // An older in-flight transition could still win the race; reassert.
      if (pending && pending.to !== href) {
        pendingNavigation.current = null;
        router.replace(href);
      }
      return;
    }
    pendingNavigation.current = { from: currentHrefRef.current, to: href };
    router.replace(href);
  }, [router]);
  const controllerValue: SessionTabsController = {
    state: tabsState,
    canBack: !!tabsState && canTravelSessionTab(tabsState, -1),
    canForward: !!tabsState && canTravelSessionTab(tabsState, 1),
    select(id: string) {
      const state = tabsRef.current;
      if (!state || state.active === id) return;
      const next = selectSessionTab(state, id);
      applyTabs(next);
      const entry = currentSessionEntry(next);
      if (entry) navigateTab(entry.href);
    },
    close(id: string) {
      const state = tabsRef.current;
      if (!state) return;
      const next = closeSessionTab(state, id, newTabId());
      applyTabs(next);
      if (state.active === id) {
        const entry = currentSessionEntry(next);
        if (entry) navigateTab(entry.href);
      }
    },
    back() { controllerValue.travel(-1); },
    forward() { controllerValue.travel(1); },
    openTab() {
      const state = tabsRef.current;
      if (!state) return;
      const id = newTabId();
      const href = `/app?compose=1&draftTab=${encodeURIComponent(id)}`;
      applyTabs(openSessionTab(state, href, "Home", id));
      navigateTab(href);
    },
    openBackground(href: string) {
      const state = tabsRef.current;
      const clean = sanitizeSessionHref(href);
      if (!state || !clean) return;
      applyTabs(appendSessionTab(state, clean, titleForHref(clean), newTabId()));
    },
    travel(delta: number) {
      const state = tabsRef.current;
      if (!state) return;
      const next = travelSessionTab(state, delta);
      if (next === state) return;
      applyTabs(next);
      const entry = currentSessionEntry(next);
      if (entry) navigateTab(entry.href);
    },
  };
  return <WorkspaceContext.Provider value={{ ...overview, releaseUpdate, conversationCache, prefetchConversation, collapsed, setCollapsed, setPageTitle, connectAgent: () => setConnectionOpen(true) }}><SessionTabsContext.Provider value={controllerValue}><title>{`${pageTitle} — Canter`}</title>{children}{connectionOpen && overview.data ? <AgentConnectionDialog workspaceId={overview.data.workspace.id} defaultAuthority={overview.data.workspace.agentAuthority} onClose={() => setConnectionOpen(false)} onConnected={overview.retry} /> : null}</SessionTabsContext.Provider></WorkspaceContext.Provider>;
}

export function useWorkspace() {
  const value = useContext(WorkspaceContext);
  if (!value) throw new Error("WorkspaceProvider is required.");
  return value;
}
