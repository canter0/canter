"use client";

import Link from "next/link";
import { useState } from "react";
import { WorkspaceIcon, type WorkspaceIconName } from "./workspace-icon";
import { useWorkspace } from "./workspace-context";
import styles from "./settings.module.css";

const groups: { title: string; items: { label: string; href: string; icon: WorkspaceIconName }[] }[] = [
  { title: "Personal", items: [{ label: "Profile", href: "/app/account", icon: "agent" }, { label: "Connections", href: "/app/account/connections", icon: "apps" }] },
  { title: "Workspace", items: [{ label: "General", href: "/app/settings", icon: "settings" }, { label: "Plans", href: "/app/billing?view=plans", icon: "panel" }, { label: "Invoices", href: "/app/billing?view=invoices", icon: "file" }, { label: "Usage", href: "/app/billing", icon: "activity" }] },
  { title: "Resources & access", items: [{ label: "Secrets", href: "/app/settings/secrets", icon: "lock" }, { label: "Agents", href: "/app/settings/agent", icon: "agent" }] },
];

export function SettingsNavigation({ active }: { active: string }) {
  const [search, setSearch] = useState("");
  const { data } = useWorkspace();
  const filtered = groups.map(group => ({ ...group, items: group.items.filter(item => `${group.title} ${item.label}`.toLowerCase().includes(search.toLowerCase())) }));
  return <nav className={styles.navigation} aria-label="Settings navigation">
    <Link href="/app" className={styles.back}>← Back to workspace</Link>
    <label className={styles.search}><WorkspaceIcon name="search" width="15" height="15" /><input aria-label="Search settings" placeholder="Search settings…" value={search} onChange={event => setSearch(event.target.value)} /></label>
    {filtered.map(group => group.items.length ? <div className={styles.navGroup} key={group.title}><h2>{group.title === "Workspace" ? data?.workspace.name ?? group.title : group.title}</h2>{group.items.map(item => <Link key={item.href} href={item.href} aria-current={active === item.label ? "page" : undefined}><WorkspaceIcon name={item.icon} width="16" height="16" /><span>{item.label}</span></Link>)}</div> : null)}
    {!filtered.some(group => group.items.length) ? <p className={styles.muted}>No matching settings.</p> : null}
  </nav>;
}
