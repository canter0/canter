"use client";

import { ConnectAgentButton } from "./connect-agent-button";
import { useSurfaceWorkspace } from "./embedded-app-surface";
import { agentIsConnected } from "@/lib/canter-api";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { shortcutPages, shortcutLabel } from "@/lib/a-shortcuts";
import type { SpotlightCommand } from "@/lib/spotlight";
import { useAShortcuts, useShortcutPreference } from "./use-a-shortcuts";
import { SettingsNavigation } from "./settings-navigation";
import type {} from "react/canary";
import { type ReactNode, useState, useEffect, useLayoutEffect, useRef, useCallback, useId, useSyncExternalStore, ViewTransition } from "react";
import { usePopover } from "./use-popover";
import { useMediaQuery } from "./use-media-query";
import { useFocusContainment } from "./use-focus-containment";
import { moveMenuFocus } from "@/lib/interaction";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon, type WorkspaceIconName } from "./workspace-icon";
import { WorkspaceLoading } from "./workspace-loading";
import { ConversationList } from "./conversation-list";
import { ConversationSearch } from "./conversation-search";
import { ReleaseUpdateNotice } from "./release-update-notice";
import { MorphLabel } from "./conversation-motion";
import { MotionNav } from "./motion-nav";
import { MotionPresence } from "./motion-presence";
import styles from "./workspace.module.css";

function subscribeToPageFocus(onChange: () => void) {
  window.addEventListener("focus", onChange);
  window.addEventListener("blur", onChange);
  document.addEventListener("visibilitychange", onChange);
  return () => {
    window.removeEventListener("focus", onChange);
    window.removeEventListener("blur", onChange);
    document.removeEventListener("visibilitychange", onChange);
  };
}

function pageIsFocused() {
  return document.visibilityState !== "hidden" && document.hasFocus();
}

function serverPageIsFocused() { return true; }

type NavItem = "Home" | "Task" | "System" | "Changes" | "Agents" | "Account" | "Billing";
const navigation: Array<{ label: string; active: NavItem; href: string; icon: WorkspaceIconName; key: string }> = [
  { label: "New conversation", active: "Home", href: "/app", icon: "plus", key: "n" },
  { label: "Apps", active: "System", href: "/app/system", icon: "apps", key: "s" },
  { label: "Activity", active: "Changes", href: "/app/changes", icon: "activity", key: "d" },
  { label: "Agents", active: "Agents", href: "/app/agents", icon: "agent", key: "g" },
];

export function AppShell({ active, context, children, onNewInstruction, agentView, settingsNavigation, pageTitle, workspaceCommands = [], onboarding = false, onboardingTransition = false }: { active: NavItem; context?: string; children: ReactNode; onNewInstruction?: () => void; agentView?: boolean; settingsNavigation?: ReactNode; pageTitle?: string; workspaceCommands?: SpotlightCommand[]; onboarding?: boolean; onboardingTransition?: boolean }) {
  const { data, loading, unavailable, error, retry, collapsed, setCollapsed, setPageTitle, releaseUpdate } = useWorkspace();
  const embedded = useSurfaceWorkspace();
  const pageFocused = useSyncExternalStore(subscribeToPageFocus, pageIsFocused, serverPageIsFocused);
  const router = useRouter();
  const shortcuts = useShortcutPreference();
  const [accountOpen, setAccountOpen] = useState(false);
  const [searchOpen, setSearchOpen] = useState(false);
  const closeSearch = useCallback(() => setSearchOpen(false), []);
  const accountMenu = useRef<HTMLDivElement>(null);
  const sidebar = useRef<HTMLElement>(null);
  const navigationTrigger = useRef<HTMLButtonElement>(null);
  const sidebarId = useId();
  const accountId = useId();
  const closeAccount = useCallback(() => setAccountOpen(false), []);
  usePopover(accountOpen, accountMenu, closeAccount);
  const pathname = usePathname();
  const isSettings = pathname.startsWith("/app/settings") || pathname.startsWith("/app/account") || pathname.startsWith("/app/billing");
  const settingsActive = pathname.startsWith("/app/settings/agent") ? "Agents" : pathname.startsWith("/app/settings/secrets") ? "Secrets" : pathname.startsWith("/app/account/connections") ? "Connections" : pathname.startsWith("/app/account") ? "Profile" : pathname.startsWith("/app/billing") ? "Usage" : "General";
  const sidebarSettings = settingsNavigation ?? (isSettings ? <SettingsNavigation active={settingsActive} /> : null);
  const navActive = pathname.startsWith("/app/conversations/") ? "Task" : pathname.startsWith("/app/agents") ? "Agents" : pathname.startsWith("/app/system") ? "System" : pathname.startsWith("/app/changes") ? "Changes" : pathname === "/app" ? "Home" : active;
  const conversationTitle = data?.conversations.find(item => pathname === `/app/conversations/${encodeURIComponent(item.id)}`)?.title;
  const title = pageTitle ?? conversationTitle ?? (isSettings ? settingsActive : { Home: "New conversation", Task: "Task", System: "Apps", Changes: "Activity", Agents: "Agents", Account: "Profile", Billing: "Billing" }[navActive]);
  useLayoutEffect(() => { if (!embedded) setPageTitle(title); }, [title, pathname, embedded, setPageTitle]);
  const [mobileOpen, setMobileOpen] = useState(false);
  const mobile = useMediaQuery("(max-width: 1023px)");
  const closeNavigation = useCallback(() => setMobileOpen(false), []);
  const navigationModal = mobile && mobileOpen;
  useFocusContainment(navigationModal, sidebar, closeNavigation, navigationTrigger);
  useEffect(() => {
    const media = window.matchMedia("(max-width: 1023px)");
    const resize = () => { closeNavigation(); if (media.matches && sidebar.current?.contains(document.activeElement)) navigationTrigger.current?.focus(); };
    media.addEventListener("change", resize);
    return () => media.removeEventListener("change", resize);
  }, [closeNavigation]);
  useEffect(() => {
    if (embedded) return;
    const keyboard = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k" && !event.altKey && !event.repeat && !event.isComposing && !document.querySelector('dialog[open], [aria-modal="true"]')) {
        event.preventDefault();
        setAccountOpen(false);
        setSearchOpen(true);
      }
    };
    window.addEventListener("keydown", keyboard);
    return () => window.removeEventListener("keydown", keyboard);
  }, [embedded]);
  function newConversation() {
    setMobileOpen(false);
    if (onNewInstruction) onNewInstruction();
    else router.push("/app?compose=1");
  }
  const commands: SpotlightCommand[] = [
    { id: "new", title: "New conversation", key: "n", icon: "plus", run: newConversation },
    { id: "sidebar", title: `${(mobile ? mobileOpen : !collapsed) ? "Close" : "Open"} left sidebar`, key: "q", icon: "panel", run: () => { setAccountOpen(false); if (mobile) setMobileOpen(value => !value); else setCollapsed(!collapsed); } },
    ...workspaceCommands,
    ...shortcutPages.map(page => ({ ...page, run: () => { setMobileOpen(false); router.push(page.href); } })),
  ];
  useAShortcuts(!embedded && !onboarding && shortcuts.enabled, key => {
    if (key === " ") { setAccountOpen(false); setSearchOpen(true); return true; }
    const command = commands.find(command => command.key === key);
    if (!command) return false;
    command.run();
    return true;
  });
  const connected = data?.installations.filter(agent => agent.harness !== "canter-hosted" && agentIsConnected(agent)) ?? [];
  const accountName = data?.account.email.split("@")[0] || "Account";

  function startNewConversation(event: { preventDefault: () => void }) {
    setMobileOpen(false);
    if (active === "Home" && onNewInstruction) {
      event.preventDefault();
      onNewInstruction();
    }
  }

  if (embedded) return <>{children}</>;

  return (
    <div className={`dashboard-theme ${styles.shell}`} data-page-inactive={!pageFocused} data-collapsed={collapsed} data-mobile-open={mobileOpen} data-agent-view={agentView} data-settings={!!sidebarSettings} data-onboarding={onboarding || undefined} data-onboarding-transition={onboardingTransition || undefined}>
      <a className={styles.skipLink} href="#workspace-main" inert={navigationModal}>Skip to content</a>
      <nav className={styles.homeRail} aria-label="Home and account" inert={navigationModal || onboarding}>
        <Link className={styles.railHome} prefetch={true} href="/app" aria-label="Home" title="Home" aria-current={!isSettings ? "location" : undefined} onNavigate={closeAccount}><WorkspaceIcon name="home" width="22" height="22" /></Link>
        <div className={styles.accountAnchor} ref={accountMenu}><button type="button" className={styles.profile} aria-label={`Account menu for ${accountName}`} aria-haspopup="menu" aria-controls={accountOpen ? accountId : undefined} aria-expanded={accountOpen} onKeyDown={event => { if (["ArrowDown", "ArrowUp"].includes(event.key)) { event.preventDefault(); setAccountOpen(true); } }} onClick={() => setAccountOpen(!accountOpen)}>
          <span className={styles.profileAvatar} aria-hidden="true">{accountName.slice(0, 1).toUpperCase()}</span>
        </button><MotionPresence open={accountOpen}><div id={accountId} className={styles.accountMenu} role="menu" aria-label="Account" onKeyDown={moveMenuFocus}>
          <p className={styles.menuLabel}>{data?.account.email ?? accountName}</p>
          <Link role="menuitem" tabIndex={-1} prefetch={true} href="/app/account" onNavigate={closeAccount}><WorkspaceIcon name="agent" />Profile</Link>
          <Link role="menuitem" tabIndex={-1} prefetch={true} href="/app/settings" onNavigate={closeAccount}><WorkspaceIcon name="settings" />Workspace settings</Link>
          <Link role="menuitem" tabIndex={-1} prefetch={true} href="/app/billing" onNavigate={closeAccount}><WorkspaceIcon name="file" />Billing</Link>
        </div></MotionPresence></div>
      </nav>
      <header className={styles.topBar} data-conversation={agentView || undefined} data-update-available={!!releaseUpdate.available || undefined} inert={navigationModal || onboarding}>
        <button ref={navigationTrigger} className={`${styles.iconButton} ${styles.mobileMenu}`} aria-label="Open navigation" aria-expanded={navigationModal} aria-controls={sidebarId} onClick={() => { setAccountOpen(false); setMobileOpen(true); }}><WorkspaceIcon name="panel" /></button>
        {agentView ? <h1 className={styles.topBarTitle} title={title}>{title}</h1> : <Link className={`wordmark ${styles.mobileWordmark}`} prefetch={true} href="/app">canter</Link>}
        <ReleaseUpdateNotice update={releaseUpdate} />
      </header>
      {navigationModal ? <div className={styles.sidebarBackdrop} aria-hidden="true" onClick={closeNavigation} /> : null}
      <ViewTransition name={sidebarSettings ? "settings-sidebar" : "dashboard-sidebar"} default="none" enter="sidebar-fade" exit="sidebar-fade">
      <aside ref={sidebar} data-shortcut-navigation inert={onboarding || (mobile && !mobileOpen)} id={sidebarId} className={styles.sidebar} role={navigationModal ? "dialog" : undefined} aria-modal={navigationModal || undefined} tabIndex={-1} aria-label={sidebarSettings ? "Settings sidebar" : "Workspace sidebar"} onClick={event => { if (event.target instanceof Element && event.target.closest("a")) setMobileOpen(false); }}>
        <div className={styles.workspaceHeading}>
          <Link className={`wordmark ${styles.sidebarWordmark}`} prefetch={true} href="/app" aria-label="Canter home">canter</Link>
          <button className={`${styles.iconButton} ${styles.desktopToggle}`} title="Toggle left sidebar · A + Q" aria-label={collapsed ? "Expand sidebar" : "Minimize sidebar"} aria-expanded={!collapsed} onClick={() => setCollapsed(!collapsed)}><WorkspaceIcon name="panel" /></button>
          <button className={`${styles.iconButton} ${styles.mobileClose}`} aria-label="Close navigation" onClick={() => setMobileOpen(false)}><WorkspaceIcon name="close" /></button>
        </div>
        {sidebarSettings ?? <><MotionNav name="workspace" className={styles.navigation} label="Main navigation">
          {navigation.map(item => <Link key={item.active} prefetch={true} href={item.href} title={`${item.label} · ${shortcutLabel(item.key)}`} aria-label={item.label} aria-current={navActive === item.active ? "page" : undefined} data-motion-nav-skip={item.active === "Home" || undefined} className={styles.navLink} onNavigate={event => {
            setMobileOpen(false);
            if (item.active === "Home") startNewConversation(event);
          }}><WorkspaceIcon name={item.icon} /><span>{item.label}</span></Link>)}
        </MotionNav>
        <div className={styles.recentSection} role="region" aria-label="Conversations" tabIndex={0}>
          <div className={styles.sidebarLabel}>
            <span>Conversations</span>
            <div className={styles.conversationActions}>
              <button type="button" className={styles.iconButton} aria-label="Search conversations" title="Spotlight · A + Space" aria-haspopup="dialog" aria-expanded={searchOpen} onClick={() => setSearchOpen(true)}><WorkspaceIcon name="search" width="16" height="16" /></button>
              <Link prefetch={true} href="/app" className={`${styles.iconButton} ${styles.newConversation}`} aria-label="New conversation" title="New conversation · A + N" onNavigate={startNewConversation}><WorkspaceIcon name="plus" /></Link>
            </div>
          </div>
          <ConversationList compact={collapsed && !sidebarSettings} />
        </div>
        <div className={styles.sidebarBottom}>
          {connected.length ? <Link href="/app/agents" aria-label={`${connected.length} agent${connected.length === 1 ? "" : "s"} with workspace access`} title="Agent connections" className={styles.agentLink}><span className={styles.connectionDot} data-connected /><span>{connected.length} agent{connected.length === 1 ? "" : "s"} with access</span><WorkspaceIcon name="external" width="14" height="14" /></Link> : <ConnectAgentButton className={styles.agentLink}><span className={styles.connectionDot} /><span>Connect your agent</span><WorkspaceIcon name="external" width="14" height="14" /></ConnectAgentButton>}
        </div></>}
      </aside>
      </ViewTransition>
      <ViewTransition key={agentView ? "conversation" : `${pathname}:${title}`} name="canter-workspace-page" default="none" share={agentView ? "none" : "canter-page"} enter={agentView ? "none" : "canter-page"} exit={agentView ? "none" : "canter-page"}>
      <main id="workspace-main" tabIndex={-1} className={styles.main} inert={navigationModal}>
        {active === "Task" ? <header className={styles.pageBar}><span>Task</span>{context && context !== "canter / default" ? <span className={styles.breadcrumb}>{context}</span> : null}</header> : null}
        {error && !unavailable ? <div className={styles.syncNotice} role="status"><span>Workspace updates are paused. Showing the last loaded data.</span><button onClick={retry}>Reconnect</button></div> : null}
        {loading ? <WorkspaceLoading variant={active === "Home" || active === "Task" ? "conversation" : active === "System" ? "cards" : "rows"} /> : error && unavailable ? <section className={styles.loadFailure} role="alert"><WorkspaceIcon name="activity" /><h1>Couldn’t load this view</h1><p>{error}</p><button className={styles.primaryButton} onClick={retry}>Try again</button></section> : children}
      </main>
      </ViewTransition>
      {searchOpen ? <ConversationSearch conversations={data?.conversations ?? []} commands={commands} shortcutsEnabled={shortcuts.enabled} onShortcutsChange={shortcuts.setEnabled} onClose={closeSearch} onNavigate={closeNavigation} /> : null}
    </div>
  );
}

export function Metric({ label, value }: { label: string; value: string }) {
  return <div className="border-l border-[var(--rule)] pl-5"><div className="meta">{label}</div><div className="mt-2 text-[22px]"><MorphLabel text={value} /></div></div>;
}

export function SectionHeader({ left, right }: { left: string; right?: string }) {
  return <div className="flex justify-between border-b border-[var(--rule-strong)] pb-3 text-[10px] tracking-[0.075em]"><span>{left}</span>{right ? <span>{right}</span> : null}</div>;
}
