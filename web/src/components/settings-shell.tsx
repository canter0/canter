"use client";

import Link from "next/link";
import { useState, type ReactNode } from "react";
import { AppShell } from "./app-shell";
import { WorkspaceIcon, type WorkspaceIconName } from "./workspace-icon";
import { useWorkspace } from "./workspace-context";
import styles from "./settings.module.css";

const groups: { title: string; items: { label: string; href: string; icon: WorkspaceIconName }[] }[] = [
  { title: "Personal", items: [{ label: "Profile", href: "/app/account", icon: "agent" }, { label: "Connections", href: "/app/account/connections", icon: "apps" }] },
  { title: "Workspace", items: [{ label: "General", href: "/app/settings", icon: "settings" }, { label: "Plans", href: "/app/billing?view=plans", icon: "panel" }, { label: "Invoices", href: "/app/billing?view=invoices", icon: "file" }, { label: "Usage", href: "/app/billing", icon: "activity" }] },
  { title: "Resources & access", items: [{ label: "Secrets", href: "/app/settings/secrets", icon: "lock" }, { label: "Agents", href: "/app/settings/agent", icon: "agent" }] },
];

export function SettingsShell({ active, title, description, children }: { active: string; title: string; description?: string; children: ReactNode }) {
  const [search, setSearch] = useState("");
  const { data } = useWorkspace();
  const filtered = groups.map(group => ({ ...group, items: group.items.filter(item => `${group.title} ${item.label}`.toLowerCase().includes(search.toLowerCase())) }));
  const navigation = <nav className={styles.navigation} aria-label="Settings navigation">
    <Link href="/app" className={styles.back}>← Back to workspace</Link>
    <label className={styles.search}><WorkspaceIcon name="search" width="15" height="15" /><input aria-label="Search settings" placeholder="Search settings…" value={search} onChange={event => setSearch(event.target.value)} /></label>
    {filtered.map(group => group.items.length ? <div className={styles.navGroup} key={group.title}><h2>{group.title === "Workspace" ? data?.workspace.name ?? group.title : group.title}</h2>{group.items.map(item => <Link key={item.href} href={item.href} aria-current={active === item.label ? "page" : undefined}><WorkspaceIcon name={item.icon} width="16" height="16" /><span>{item.label}</span></Link>)}</div> : null)}
    {!filtered.some(group => group.items.length) ? <p className={styles.muted}>No matching settings.</p> : null}
  </nav>;
  return <AppShell active={["Usage", "Plans", "Invoices"].includes(active) ? "Billing" : "Account"} settingsNavigation={navigation}>
    <div className={styles.breadcrumb}><Link href="/app/settings">Settings</Link><WorkspaceIcon name="chevron" width="12" height="12" /><span>{title}</span></div>
    <div className={styles.page}><header className={styles.heading}><h1>{title}</h1>{description ? <p>{description}</p> : null}</header>{children}</div>
  </AppShell>;
}

export function SettingRow({ title, description, children }: { title: string; description?: string; children: ReactNode }) {
  return <div className={styles.row}><div><h3>{title}</h3>{description ? <p>{description}</p> : null}</div><div className={styles.rowValue}>{children}</div></div>;
}
