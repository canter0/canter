"use client";

import Link from "next/link";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { startTransition, useState } from "react";
import { ConnectAgentButton } from "@/components/connect-agent-button";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { MotionNav } from "@/components/motion-nav";
import { activityLabel } from "@/lib/workspace-overview";
import { actionHref, actionTitle } from "@/lib/workspace-actions";
import { relativeTime, type WorkspaceAction } from "@/lib/canter-api";
import type { WorkspaceOverview } from "@/lib/workspace-data";
import { activityStatuses, buildActivityTimeline, type ActivityStatus, type TimelineEntry } from "@/lib/activity-timeline";
import styles from "@/components/workspace.module.css";
import activity from "@/components/activity.module.css";

const statusLabels: Record<ActivityStatus, string> = { all: "All", attention: "Needs review", active: "In progress", completed: "Completed", failed: "Failed", declined: "Declined" };

function actorName(action: WorkspaceAction) {
  return action.actor.displayName || (action.actor.kind === "agent" ? "Agent" : action.actor.kind === "human" ? "User" : "Canter");
}

function EventRow({ action }: { action: WorkspaceAction }) {
  return <div className={activity.timelineEvent} data-failed={action.metadata.outcome === "failed" || undefined}>
    <span className={activity.timelineTitle}>{actionTitle(action)}</span>
    <span className={activity.timelineActor}>{actorName(action)}</span>
    <time className={activity.timelineTime} dateTime={action.occurredAt} title={new Date(action.occurredAt).toLocaleString()}>{relativeTime(action.occurredAt)}</time>
  </div>;
}

function HistoryRow({ entry }: { entry: TimelineEntry }) {
  const [open, setOpen] = useState(false);
  const timelineId = `events-${entry.kind}-${entry.id}`;
  return <>
    <div className={activity.historyRow}>
      <WorkspaceIcon name={entry.kind === "Deployment" ? "apps" : "activity"} />
      <div className={activity.historyBody}>
        <Link href={entry.href} className={activity.historySummary}>{entry.summary}</Link>
        <span className={activity.historyMeta}>{entry.system} · {entry.kind}{entry.actor ? ` · ${entry.actor}` : ""}{entry.events.length ? ` · ${entry.events.length} ${entry.events.length === 1 ? "event" : "events"}` : ""}</span>
      </div>
      <span className={`${styles.status} ${activity.historyStatus}`} data-phase={entry.phase}>{activityLabel(entry.phase)}</span>
      {entry.at ? <time className={activity.historyTime} dateTime={entry.at} title={new Date(entry.at).toLocaleString()}>{relativeTime(entry.at)}</time> : null}
      {entry.events.length ? <button type="button" className={activity.expandButton} aria-expanded={open} aria-controls={timelineId} aria-label={open ? "Hide events" : "Show events"} onClick={() => setOpen(value => !value)}><WorkspaceIcon name="chevron" width="14" height="14" /></button> : null}
    </div>
    {entry.events.length ? <div className={activity.timeline} id={timelineId} hidden={!open}>{entry.events.map(action => <EventRow key={action.id} action={action} />)}</div> : null}
  </>;
}

function EventLine({ action }: { action: WorkspaceAction }) {
  const href = actionHref(action);
  const content = <>
    <span className={activity.eventDot} aria-hidden="true" />
    <div>{actionTitle(action)} <span className={activity.eventActor}>{actorName(action)}{action.metadata.system ? ` · ${action.metadata.system}` : ""}</span></div>
    {action.metadata.outcome === "failed" ? <span className={styles.status} data-phase="failed">Failed</span> : null}
    <time dateTime={action.occurredAt} title={new Date(action.occurredAt).toLocaleString()}>{relativeTime(action.occurredAt)}</time>
  </>;
  return href ? <Link href={href} className={activity.eventLine}>{content}</Link> : <div className={activity.eventLine}>{content}</div>;
}

export function ActivityView({ data }: { data: Pick<WorkspaceOverview, "changes" | "initialDeployments" | "actions"> }) {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const rawStatus = searchParams.get("status");
  const status: ActivityStatus = activityStatuses.includes(rawStatus as ActivityStatus) ? rawStatus as ActivityStatus : "all";
  const systems = [...new Set([...data.changes, ...data.initialDeployments].map(item => item.system))].sort();
  const rawSystem = searchParams.get("app") ?? "";
  const system = systems.includes(rawSystem) ? rawSystem : "";
  const reads = searchParams.get("reads") === "1";
  const timeline = buildActivityTimeline(data, { showReads: reads, status, system });

  const update = (next: Partial<{ status: ActivityStatus; system: string; reads: boolean }>) => {
    const merged = { status, system, reads, ...next };
    const params = new URLSearchParams();
    if (merged.status !== "all") params.set("status", merged.status);
    if (merged.system) params.set("app", merged.system);
    if (merged.reads) params.set("reads", "1");
    const query = params.toString();
    startTransition(() => router.replace(query ? `${pathname}?${query}` : pathname, { scroll: false }));
  };

  return <>
    <div className={activity.filterBar}>
      <MotionNav name="activity-status" className={styles.filters} role="group" label="Filter activity">
        {activityStatuses.map(item => <button key={item} type="button" aria-pressed={status === item} onClick={() => update({ status: item })}>{statusLabels[item]}</button>)}
      </MotionNav>
      {timeline.systems.length > 1 ? <select className={activity.appSelect} aria-label="Filter by app" value={system} onChange={event => update({ system: event.target.value })}>
        <option value="">All apps</option>
        {timeline.systems.map(name => <option key={name} value={name}>{name}</option>)}
      </select> : null}
      <button type="button" className={activity.readToggle} aria-pressed={reads} onClick={() => update({ reads: !reads })}>Show agent reads</button>
    </div>
    {timeline.counts.total === 0 ? <div className={styles.emptyState}><span className={styles.emptyIcon}><WorkspaceIcon name="activity" width="24" height="24" /></span><h2>No activity yet</h2><p>When your agent prepares a deployment or a change, you can follow it here.</p><ConnectAgentButton className={styles.secondaryButton}>Bring your agent</ConnectAgentButton></div>
    : timeline.counts.shown === 0 ? <div className={styles.emptyState}><span className={styles.emptyIcon}><WorkspaceIcon name="search" width="24" height="24" /></span><h2>No matching activity</h2><p>Nothing matches these filters.</p><button type="button" className={styles.secondaryButton} onClick={() => update({ status: "all", system: "", reads: false })}>Clear filters</button></div>
    : <>
      {timeline.attention.length ? <section className={activity.section} aria-labelledby="activity-review">
        <div className={activity.sectionHeading}><h2 id="activity-review">Needs your review</h2><small>{timeline.attention.length}</small></div>
        <div className={activity.reviewList}>{timeline.attention.map(entry => <div key={`${entry.kind}-${entry.id}`} className={activity.reviewCard}>
          <div className={activity.reviewBody}>
            <h3>{entry.summary}</h3>
            <p>{entry.system} · {entry.kind}{entry.actor ? ` · prepared by ${entry.actor}` : ""}{entry.at ? ` · ${relativeTime(entry.at)}` : ""}</p>
          </div>
          <Link href={entry.href} className={`${styles.secondaryButton} ${activity.reviewButton}`}>Review</Link>
        </div>)}</div>
      </section> : null}
      {timeline.inProgress.length ? <section className={activity.section} aria-labelledby="activity-progress">
        <div className={activity.sectionHeading}><h2 id="activity-progress">In progress</h2><small>{timeline.inProgress.length}</small></div>
        <div className={activity.progressList}>{timeline.inProgress.map(entry => <Link key={`${entry.kind}-${entry.id}`} href={entry.href} className={activity.progressRow}>
          <span className={activity.pulseDot} aria-hidden="true" />
          <div className={activity.progressBody}><span>{entry.summary}</span><small>{entry.system} · {entry.kind}{entry.at ? ` · ${relativeTime(entry.at)}` : ""}</small></div>
          <span className={styles.status} data-phase={entry.phase}>{activityLabel(entry.phase)}</span>
        </Link>)}</div>
      </section> : null}
      {timeline.history.length ? <section className={activity.section} aria-labelledby="activity-history">
        <div className={activity.sectionHeading}><h2 id="activity-history">History</h2></div>
        {timeline.history.map(group => <div key={group.key} className={activity.dayGroup}>
          <h3 className={activity.dayHeading}>{group.label}</h3>
          {group.items.map(item => item.type === "item" ? <HistoryRow key={`${item.kind}-${item.id}`} entry={item} /> : <EventLine key={item.action.id} action={item.action} />)}
        </div>)}
      </section> : null}
    </>}
  </>;
}
