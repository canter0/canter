"use client";

import { ConnectAgentButton } from "./connect-agent-button";
import { useSurfaceWorkspace } from "./embedded-app-surface";
import { agentIsConnected } from "@/lib/canter-api";

import Link from "next/link";
import { type ReactNode, useState, useEffect, useLayoutEffect, useRef } from "react";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon, type WorkspaceIconName } from "./workspace-icon";
import { WorkspaceLoading } from "./workspace-loading";
import styles from "./workspace.module.css";

type NavItem = "Home" | "Task" | "System" | "Changes" | "Agents" | "Account" | "Billing";
const navigation: Array<{ label: string; active: NavItem; href: string; icon: WorkspaceIconName }> = [
  { label: "New conversation", active: "Home", href: "/app", icon: "plus" },
  { label: "Apps", active: "System", href: "/app/system", icon: "apps" },
  { label: "Activity", active: "Changes", href: "/app/changes", icon: "activity" },
  { label: "Agents", active: "Agents", href: "/app/settings/agent", icon: "agent" },
];

export function AppShell({ active, context, children, onNewInstruction, agentView, settingsNavigation }: { active: NavItem; context?: string; children: ReactNode; onNewInstruction?: () => void; agentView?: boolean; settingsNavigation?: ReactNode }) {
  const { data, loading, collapsed, setCollapsed, navPositionRef } = useWorkspace();
  const embedded = useSurfaceWorkspace();
  const [accountOpen, setAccountOpen] = useState(false);
  const accountMenu = useRef<HTMLDivElement>(null);
  const indicator = useRef<HTMLSpanElement>(null);
  const navIndex = navigation.findIndex(item => item.active === active);
  useLayoutEffect(() => {
    if (navIndex < 0) return;
    if (!window.matchMedia("(prefers-reduced-motion: reduce)").matches) indicator.current?.animate([{ transform: `translateY(${navPositionRef.current * 42}px)` }, { transform: `translateY(${navIndex * 42}px)` }], { duration: 260, easing: "cubic-bezier(.2,.8,.2,1)" });
    navPositionRef.current = navIndex;
  }, [navIndex, navPositionRef]);
  useEffect(() => {
    if (!accountOpen) return;
    const outside = (event: PointerEvent) => { if (event.target instanceof Node && !accountMenu.current?.contains(event.target)) setAccountOpen(false); };
    const escape = (event: KeyboardEvent) => { if (event.key === "Escape") { setAccountOpen(false); accountMenu.current?.querySelector("button")?.focus(); } };
    document.addEventListener("pointerdown", outside); document.addEventListener("keydown", escape);
    return () => { document.removeEventListener("pointerdown", outside); document.removeEventListener("keydown", escape); };
  }, [accountOpen]);
  const [mobileOpen, setMobileOpen] = useState(false);
  const connected = data?.installations.filter(agent => agent.harness !== "canter-hosted" && agentIsConnected(agent)) ?? [];
  const accountName = data?.account.email.split("@")[0] || "Account";

  if (embedded) return <>{children}</>;

  return (
    <div className={`dashboard-theme ${styles.shell}`} data-collapsed={collapsed && !settingsNavigation} data-mobile-open={mobileOpen} data-agent-view={agentView} data-settings={!!settingsNavigation}>
      <header className={styles.topBar}>
        <button className={`${styles.iconButton} ${styles.mobileMenu}`} aria-label="Open navigation" aria-expanded={mobileOpen} onClick={() => setMobileOpen(true)}><WorkspaceIcon name="panel" /></button>
        <Link className={`wordmark ${styles.mobileWordmark}`} href="/app">canter</Link>
        <div className={styles.accountAnchor} ref={accountMenu}><button type="button" className={styles.profile} aria-label={`Account menu for ${accountName}`} aria-expanded={accountOpen} onClick={() => setAccountOpen(!accountOpen)}>
          <span className={styles.profileAvatar} aria-hidden="true">{accountName.slice(0, 1).toUpperCase()}</span>
          <span className={styles.profileName}>{accountName}</span>
          <WorkspaceIcon name="down" width="12" height="12" />
        </button>{accountOpen ? <nav className={styles.accountMenu} aria-label="Account">
          <Link href="/app/account">Profile</Link><Link href="/app/settings">Workspace settings</Link><Link href="/app/billing">Billing</Link>
        </nav> : null}</div>
      </header>
      {mobileOpen ? <button className={styles.sidebarBackdrop} aria-label="Close navigation" onClick={() => setMobileOpen(false)} /> : null}
      <aside className={styles.sidebar} aria-label={settingsNavigation ? "Settings sidebar" : "Workspace sidebar"} onClick={event => { if (event.target instanceof Element && event.target.closest("a")) setMobileOpen(false); }}>
        <div className={styles.workspaceHeading}>
          <Link className={`wordmark ${styles.sidebarWordmark}`} href="/app" aria-label="Canter home">canter</Link>
          <button className={`${styles.iconButton} ${styles.desktopToggle}`} aria-label={collapsed ? "Expand sidebar" : "Minimize sidebar"} aria-expanded={!collapsed} onClick={() => setCollapsed(!collapsed)}><WorkspaceIcon name="panel" /></button>
          <button className={`${styles.iconButton} ${styles.mobileClose}`} aria-label="Close navigation" onClick={() => setMobileOpen(false)}><WorkspaceIcon name="close" /></button>
        </div>
        {settingsNavigation ?? <><nav className={styles.navigation} aria-label="Main navigation">
          {navIndex >= 0 ? <span ref={indicator} className={styles.navIndicator} style={{ transform: `translateY(${navIndex * 42}px)` }} /> : null}
          {navigation.map(item => <Link key={item.active} href={item.href} title={item.label} aria-label={item.label} aria-current={active === item.active ? "page" : undefined} className={styles.navLink} onClick={event => {
            setMobileOpen(false);
            if (item.active === "Home" && active === "Home" && onNewInstruction) { event.preventDefault(); onNewInstruction(); }
          }}><WorkspaceIcon name={item.icon} /><span>{item.label}</span></Link>)}
        </nav>
        <div className={styles.recentSection} role="region" aria-label="Conversations" tabIndex={0}>
          <div className={styles.sidebarLabel}>Conversations</div>
          {data?.conversations.length ? data.conversations.map(item => <Link key={item.id} className={styles.recentLink} aria-label={item.title} href={`/app/conversations/${encodeURIComponent(item.id)}`} title={item.title}><span>{item.title}</span><small>{["queued", "running"].includes(item.status) ? "Working…" : item.status === "failed" ? "Needs attention" : ""}</small></Link>) : <p className={styles.sidebarEmpty}>Your conversations will appear here.</p>}
        </div>
        <div className={styles.sidebarBottom}>
          {connected.length ? <Link href="/app/settings/agent" aria-label={`${connected.length} agents authorized`} title="Agent connections" className={styles.agentLink}><span className={styles.connectionDot} data-connected /><span>{connected.length} agent{connected.length === 1 ? "" : "s"} connected</span><WorkspaceIcon name="external" width="14" height="14" /></Link> : <ConnectAgentButton className={styles.agentLink}><span className={styles.connectionDot} /><span>Connect your agent</span><WorkspaceIcon name="external" width="14" height="14" /></ConnectAgentButton>}
          <Link href="/app/billing" title="Billing" aria-label="Billing" className={styles.navLink} aria-current={active === "Billing" ? "page" : undefined}><WorkspaceIcon name="file" /><span>Billing</span></Link>
          <Link href="/app/account" title="Settings" aria-label="Settings" className={styles.navLink} aria-current={active === "Account" ? "page" : undefined}><WorkspaceIcon name="settings" /><span>Settings</span></Link>
        </div></>}
      </aside>
      <main className={styles.main}>
        {active === "Task" ? <header className={styles.pageBar}><span>Task</span>{context && context !== "canter / default" ? <span className={styles.breadcrumb}>{context}</span> : null}</header> : null}
        {loading ? <WorkspaceLoading variant={active === "Home" || active === "Task" ? "conversation" : active === "System" ? "cards" : "rows"} /> : children}
      </main>
    </div>
  );
}

export function Metric({ label, value }: { label: string; value: string }) {
  return <div className="border-l border-[var(--rule)] pl-5"><div className="meta">{label}</div><div className="mt-2 text-[22px]">{value}</div></div>;
}

export function SectionHeader({ left, right }: { left: string; right?: string }) {
  return <div className="flex justify-between border-b border-[var(--rule-strong)] pb-3 text-[10px] tracking-[0.075em]"><span>{left}</span>{right ? <span>{right}</span> : null}</div>;
}
