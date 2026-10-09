"use client";

import Link from "next/link";
import { useRef, useState } from "react";
import { WorkspaceIcon, type WorkspaceIconName } from "./workspace-icon";
import { useWorkspace } from "./workspace-context";
import { MotionNav } from "./motion-nav";
import styles from "./settings.module.css";

const groups: { title: string; items: { label: string; href: string; icon: WorkspaceIconName }[] }[] = [
  { title: "Personal", items: [{ label: "Profile", href: "/app/account", icon: "agent" }, { label: "Security", href: "/app/account/security", icon: "lock" }, { label: "Connections", href: "/app/account/connections", icon: "apps" }] },
  { title: "Workspace", items: [{ label: "General", href: "/app/settings", icon: "settings" }, { label: "Plans", href: "/app/billing?view=plans", icon: "panel" }, { label: "Invoices", href: "/app/billing?view=invoices", icon: "file" }, { label: "Usage", href: "/app/billing", icon: "activity" }] },
  { title: "Resources & access", items: [{ label: "Secrets", href: "/app/settings/secrets", icon: "lock" }, { label: "Agents", href: "/app/settings/agent", icon: "agent" }] },
];

export function SettingsNavigation({ active }: { active: string }) {
  const [search, setSearch] = useState("");
  const input = useRef<HTMLInputElement>(null);
  const { data } = useWorkspace();
  const query = search.trim().toLowerCase();
  const filtered = groups.map(group => ({ ...group, items: group.items.filter(item => `${group.title} ${item.label}`.toLowerCase().includes(query)) }));
  const resultCount = filtered.reduce((count, group) => count + group.items.length, 0);
  return <MotionNav name="settings" className={styles.navigation} label="Settings navigation">
    <Link href="/app" className={styles.back}>← Back to workspace</Link>
    <div className={styles.search}><WorkspaceIcon name="search" width="15" height="15" /><input ref={input} type="search" aria-label="Search settings" placeholder="Search settings…" value={search} onChange={event => setSearch(event.target.value)} onKeyDown={event => { if (event.key === "Escape" && search) { event.preventDefault(); event.stopPropagation(); setSearch(""); } }} />{search ? <button className={styles.clearSearch} type="button" aria-label="Clear settings search" onClick={() => { setSearch(""); input.current?.focus(); }}><WorkspaceIcon name="close" width="14" height="14" /></button> : null}</div>
    <span className="sr-only" role="status">{query ? `${resultCount} matching settings` : ""}</span>
    {filtered.map(group => group.items.length ? <div className={styles.navGroup} key={group.title}><h2>{group.title === "Workspace" ? data?.workspace.name ?? group.title : group.title}</h2>{group.items.map(item => <Link key={item.href} href={item.href} aria-current={active === item.label ? "page" : undefined}><WorkspaceIcon name={item.icon} width="16" height="16" /><span>{item.label}</span></Link>)}</div> : null)}
    {!resultCount ? <div className={styles.searchEmpty}><p>No settings match “{search.trim()}”.</p><button type="button" onClick={() => { setSearch(""); input.current?.focus(); }}>Clear search</button></div> : null}
  </MotionNav>;
}
