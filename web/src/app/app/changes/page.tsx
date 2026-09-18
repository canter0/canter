"use client";

import { ConnectAgentButton } from "@/components/connect-agent-button";

import Link from "next/link";
import { useState } from "react";
import { ActionFeed } from "@/components/action-feed";
import { AppShell } from "@/components/app-shell";
import { useWorkspace } from "@/components/workspace-context";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { activityLabel, workspaceActivities } from "@/lib/workspace-overview";
import { relativeTime } from "@/lib/canter-api";
import styles from "@/components/workspace.module.css";

const filters = ["All", "Needs review", "In progress", "Finished"] as const;
const runningPhases = new Set(["authorized", "queued", "running", "applying", "verifying", "compensating"]);
const finishedPhases = new Set(["committed", "succeeded", "failed", "rejected", "reverted"]);

export default function ActivityPage() {
  const { data, error, loading, retry } = useWorkspace();
  const [view, setView] = useState<"actions" | "deployments">("actions");
  const [filter, setFilter] = useState<typeof filters[number]>("All");
  const activities = data ? workspaceActivities(data) : [];
  const visible = activities.filter(item => filter === "All" || (filter === "Needs review" ? ["drafted", "escalated"].includes(item.phase) : filter === "In progress" ? runningPhases.has(item.phase) : finishedPhases.has(item.phase)));

  return <AppShell active="Changes"><section className={styles.contentPage}>
    <div className={styles.pageHeading}><div><h1>Activity</h1><p>Actions recorded in your workspace.</p></div></div>
    <div className={styles.filters}><button aria-pressed={view === "actions"} onClick={() => setView("actions")}>Actions</button><button aria-pressed={view === "deployments"} onClick={() => setView("deployments")}>Deployments and changes</button></div>
    {loading ? <p role="status" className={styles.loading}>Loading activity…</p> : error ? <div role="alert" className={styles.error}>Couldn’t load activity. <button onClick={retry}>Try again</button></div> : <>
      {view === "actions" ? <><ActionFeed actions={data?.actions ?? []} />{!data?.actions.length ? <div className={styles.emptyState}><span className={styles.emptyIcon}><WorkspaceIcon name="activity" width="24" height="24" /></span><h2>No activity yet</h2><p>Actions from connected agents will appear here.</p><ConnectAgentButton className={styles.secondaryButton}>Bring your agent</ConnectAgentButton></div> : null}</> : <>
      {activities.length > 0 ? <div className={styles.filters} aria-label="Filter activity">{filters.map(item => <button key={item} onClick={() => setFilter(item)} aria-pressed={filter === item}>{item}</button>)}</div> : null}
      {visible.length ? <div>{visible.map(item => <Link key={`${item.kind}-${item.id}`} href={item.href} className={styles.activityRow}><WorkspaceIcon name={item.kind === "Deployment" ? "apps" : "activity"} /><div><span>{item.summary}</span><small>{item.system} · {item.kind}{item.createdAt ? ` · ${relativeTime(item.createdAt)}` : ""}</small></div><span className={styles.status} data-phase={item.phase}>{activityLabel(item.phase)}</span><WorkspaceIcon name="chevron" width="14" height="14" /></Link>)}</div> : <div className={styles.emptyState}><span className={styles.emptyIcon}><WorkspaceIcon name="activity" width="24" height="24" /></span><h2>{activities.length ? "You’re all caught up" : "No activity yet"}</h2><p>{activities.length ? "No activity matches this filter." : "When your agent prepares a deployment or a change, you can follow it here."}</p>{activities.length ? <button className={styles.secondaryButton} onClick={() => setFilter("All")}>View all activity</button> : <Link href="/app" className={styles.secondaryButton}>New task<WorkspaceIcon name="right" width="15" height="15" /></Link>}</div>}
    </>}
    </>}
  </section></AppShell>;
}
