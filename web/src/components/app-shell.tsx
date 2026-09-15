"use client";

import { ConnectAgentButton } from "./connect-agent-button";
import { useSurfaceWorkspace } from "./embedded-app-surface";
import { agentIsConnected } from "@/lib/canter-api";

import Link from "next/link";
import { type ReactNode, useState } from "react";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon, type WorkspaceIconName } from "./workspace-icon";
import { taskStatus } from "@/lib/task-options";
import styles from "./workspace.module.css";

type NavItem = "Home" | "Task" | "System" | "Changes" | "Agents" | "Account";
const navigation: Array<{ label: string; active: NavItem; href: string; icon: WorkspaceIconName }> = [
  { label: "New conversation", active: "Home", href: "/app", icon: "plus" },
  { label: "Apps", active: "System", href: "/app/system", icon: "apps" },
  { label: "Activity", active: "Changes", href: "/app/changes", icon: "activity" },
  { label: "Agents", active: "Agents", href: "/app/agents", icon: "agent" },
];

export function AppShell({ active, context, children, onNewInstruction, agentView }: { active: NavItem; context?: string; children: ReactNode; onNewInstruction?: () => void; agentView?: boolean }) {
  const { data } = useWorkspace();
  const embedded = useSurfaceWorkspace();
  const [collapsed, setCollapsed] = useState(false);
  const [mobileOpen, setMobileOpen] = useState(false);
  const workspaceName = !data?.workspace.name || data.workspace.name === "default" ? "Your workspace" : data.workspace.name;
  const recent = data?.tasks.slice(0, 8) ?? [];
  const connected = data?.installations.filter(agentIsConnected) ?? [];
  const pageName = active === "Task" ? "Task" : active === "Account" ? "Settings" : navigation.find(item => item.active === active)?.label;

  if (embedded) return <>{children}</>;

  return (
    <div className={`dashboard-theme ${styles.shell}`} data-collapsed={collapsed} data-mobile-open={mobileOpen} data-agent-view={agentView}>
      <div className={styles.mobileBar}>
        <button className={styles.iconButton} aria-label="Open navigation" aria-expanded={mobileOpen} onClick={() => setMobileOpen(true)}><WorkspaceIcon name="panel" /></button>
        <Link className="wordmark" href="/app">canter</Link>
      </div>
      {mobileOpen ? <button className={styles.sidebarBackdrop} aria-label="Close navigation" onClick={() => setMobileOpen(false)} /> : null}
      <aside className={styles.sidebar} aria-label="Workspace sidebar">
        <div className={styles.workspaceHeading}>
          <span className={styles.workspaceAvatar}>{workspaceName.charAt(0).toUpperCase()}</span>
          <span className={styles.workspaceName}>{workspaceName}</span>
          <button className={`${styles.iconButton} ${styles.desktopToggle}`} aria-label="Collapse sidebar" onClick={() => setCollapsed(true)}><WorkspaceIcon name="panel" /></button>
          <button className={`${styles.iconButton} ${styles.mobileClose}`} aria-label="Close navigation" onClick={() => setMobileOpen(false)}><WorkspaceIcon name="close" /></button>
        </div>
        <nav className={styles.navigation} aria-label="Main navigation">
          {navigation.map(item => <Link key={item.active} href={item.href} aria-current={active === item.active ? "page" : undefined} className={styles.navLink} onClick={event => {
            setMobileOpen(false);
            if (item.active === "Home" && active === "Home" && onNewInstruction) { event.preventDefault(); onNewInstruction(); }
          }}><WorkspaceIcon name={item.icon} /><span>{item.label}</span></Link>)}
        </nav>
        <div className={styles.recentSection}>
          <div className={styles.sidebarLabel}>Conversations</div>
          {data?.conversations.length ? data.conversations.map(item => <Link key={item.id} className={styles.recentLink} href={`/app/conversations/${encodeURIComponent(item.id)}`} title={item.title}><span>{item.title}</span><small>{["queued", "running"].includes(item.status) ? "Working…" : item.status === "failed" ? "Needs attention" : ""}</small></Link>) : <p className={styles.sidebarEmpty}>Your conversations will appear here.</p>}
        </div>
        {recent.length ? <div className={styles.recentSection}>
          <div className={styles.sidebarLabel}>External agent tasks</div>
          {recent.length ? recent.map(item => <Link key={item.id} className={styles.recentLink} href={`/app/tasks/${encodeURIComponent(item.id)}`} title={item.prompt}><span>{item.prompt}</span><small>{taskStatus[item.status]}</small></Link>) : <p className={styles.sidebarEmpty}>No tasks yet.</p>}
        </div> : null}
        <div className={styles.sidebarBottom}>
          {connected.length ? <Link href="/app/agents" className={styles.agentLink}><span className={styles.connectionDot} data-connected /><span>{connected.length} agent{connected.length === 1 ? "" : "s"} connected</span><WorkspaceIcon name="external" width="14" height="14" /></Link> : <ConnectAgentButton className={styles.agentLink}><span className={styles.connectionDot} /><span>Connect your agent</span><WorkspaceIcon name="external" width="14" height="14" /></ConnectAgentButton>}
          <Link href="/app/billing" className={styles.navLink}><WorkspaceIcon name="file" /><span>Billing</span></Link>
          <Link href="/app/account" className={styles.navLink} aria-current={active === "Account" ? "page" : undefined}><WorkspaceIcon name="settings" /><span>Settings</span></Link>
        </div>
      </aside>
      <main className={styles.main}>
        {collapsed ? <button className={`${styles.iconButton} ${styles.reopen}`} aria-label="Expand sidebar" onClick={() => setCollapsed(false)}><WorkspaceIcon name="panel" /></button> : null}
        {active === "Task" || active === "Account" ? <header className={styles.pageBar}><span>{pageName}</span>{context && context !== "canter / default" ? <span className={styles.breadcrumb}>{context}</span> : null}</header> : null}
        {children}
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
