"use client";

import Link from "next/link";
import { useState } from "react";
import { AppShell } from "@/components/app-shell";
import { useWorkspace } from "@/components/workspace-context";
import { WorkspaceIcon } from "@/components/workspace-icon";
import styles from "@/components/workspace.module.css";

export default function AppsPage() {
  const { data, error, loading, retry } = useWorkspace();
  const [search, setSearch] = useState("");
  const systems = data?.systems ?? [];
  const query = search.trim().toLowerCase();
  const visible = systems.filter(system => `${system.contract.metadata.name} ${system.contract.spec.intent ?? ""}`.toLowerCase().includes(query));

  return <AppShell active="System"><section className={styles.contentPage}>
    <div className={styles.pageHeading}><div><h1>Your apps</h1><p>Everything you’re running with Canter.</p></div><Link href="/app" className={styles.primaryButton}><WorkspaceIcon name="plus" width="16" height="16" />Deploy an app</Link></div>
    {loading ? <p role="status" className={styles.loading}>Loading your apps…</p> : error ? <div role="alert" className={styles.error}>Couldn’t load your apps. <button onClick={retry}>Try again</button></div> : <>
      {systems.length > 0 ? <label className={styles.search}><WorkspaceIcon name="search" width="16" height="16" /><input aria-label="Search apps" placeholder="Search apps…" value={search} onChange={event => setSearch(event.target.value)} /></label> : null}
      {visible.length > 0 ? <div className={styles.cardGrid}>{visible.map(system => <Link key={system.contract.metadata.name} href={`/app/system/${encodeURIComponent(system.contract.metadata.name)}`} className={styles.resourceCard}><div className={styles.resourceCardHeader}><div><WorkspaceIcon name="apps" /><h2>{system.contract.metadata.name}</h2></div><WorkspaceIcon name="external" width="16" height="16" /></div><p>{system.contract.spec.intent || "View this app’s services and deployments."}</p><div className={styles.resourceCardFooter}><span>{system.contract.spec.services?.length ?? 0} services</span><span>Revision {system.revision}</span></div></Link>)}</div> : <div className={styles.emptyState}><span className={styles.emptyIcon}><WorkspaceIcon name="apps" width="22" height="22" /></span><h2>{systems.length ? "No matching apps" : "No apps yet"}</h2><p>{systems.length ? "Try another name or clear your search." : "Ask your agent to deploy a repository. It will appear here."}</p>{systems.length ? <button className={styles.secondaryButton} onClick={() => setSearch("")}>Clear search</button> : <Link className={styles.secondaryButton} href="/app">New task<WorkspaceIcon name="right" width="15" height="15" /></Link>}</div>}
    </>}
  </section></AppShell>;
}
