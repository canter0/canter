"use client";
import { useEffect, useRef, useState, type ReactNode } from "react";
import Link from "next/link";
import { MorphLabel, ResponseText } from "./conversation-motion";
export { ResponseText } from "./conversation-motion";
import { operatorActivity, operatorToolLabel } from "@/lib/operator-activity";
import { operatorWebSources } from "@/lib/operator-web-sources";
import { OperatorSources } from "./operator-sources";
import { turnTimeline } from "@/lib/operator-timeline";
import { isSurface, surfaceKey, surfaceLabels, type Conversation, type OperatorEvent, type OperatorMessage, type OperatorSurface } from "@/lib/operator-api";
import { OperatorAttachments } from "./operator-attachments";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./operator-workspace.module.css";


export function OperatorMessageContext({ surface, conversations }: { surface?: OperatorSurface | null; conversations: Conversation[] }) {
  if (!surface) return null;
  const title = surface.kind === "conversation" ? conversations.find(item => item.id === surface.id)?.title ?? "Earlier conversation" : surface.path?.split("/").at(-1) ?? surface.repository ?? surface.system ?? (surface.kind === "github" ? "GitHub repositories" : surfaceLabels[surface.kind]);
  const content = <><WorkspaceIcon name={surface.kind === "conversation" ? "message" : surface.kind === "repository" || surface.kind === "github" ? "folder" : surface.kind === "apps" || surface.kind === "app" ? "apps" : "file"} width="13" height="13" /><span>Context</span><strong title={title}>{title}</strong></>;
  return surface.kind === "conversation" && surface.id ? <Link className={styles.messageContext} href={`/app/conversations/${encodeURIComponent(surface.id)}`} aria-label={`Open referenced conversation: ${title}`}>{content}</Link> : <span className={styles.messageContext} aria-label={`Context: ${title}`}>{content}</span>;
}
export function OperatorTurn({ message, answer, events, running, onSelect, conversations, inline }: { message: OperatorMessage; answer?: OperatorMessage; events: OperatorEvent[]; running: boolean; onSelect: (surface: OperatorSurface) => void; conversations: Conversation[]; inline?: ReactNode }) {
  const [expanded, setExpanded] = useState(false);
  const timeline = turnTimeline(events);
  const webSources = operatorWebSources(events);
  const activity = operatorActivity(events, running, !!answer?.content);
  const label = useActivityLabel(activity.label, running);
  const surfaces = [...new Map(events.filter(event => event.kind === "surface" && isSurface(event.data) && !["github", "compute", "storage"].includes(String(event.data.kind))).map(event => [surfaceKey(event.data as OperatorSurface), event.data as OperatorSurface])).values()];
  return <section className={styles.turn} aria-label="Conversation turn" data-live={running}>
    <article className={styles.message} data-role="user"><div className={styles.messageText}>{message.content}</div><OperatorMessageContext surface={message.surface} conversations={conversations} />{message.attachments?.length ? <OperatorAttachments items={message.attachments} /> : null}</article>
    <div className={styles.agentTurn}>
      <div className={styles.operations} data-visible={running || activity.count > 0} aria-hidden={!running && !activity.count}>
        <div className={styles.activityLine} role="status" aria-live="polite">
          <button type="button" className={styles.workToggle} disabled={!activity.count} aria-expanded={activity.count ? expanded : undefined} aria-label={activity.count ? `${label}. ${expanded ? "Hide" : "Show"} action details` : undefined} onClick={() => setExpanded(value => !value)}>
            <MorphLabel text={label} shimmer={running} />
            <span className={styles.activityChevron} data-visible={activity.count > 0}><WorkspaceIcon className={styles.workChevron} name="chevron" width="12" height="12" /></span>
          </button>
        </div>
        <div className={styles.workLogReveal} data-open={expanded} aria-hidden={!expanded} inert={!expanded}><div className={styles.workLogClip}><div className={styles.workLog}>
          {timeline.work.map(event => event.kind === "text" ? <ResponseText key={event.sequence} text={event.data.content} /> : <div key={String(event.data.callId)} className={styles.toolRow}>
            <span className={styles.toolIcon}>{event.data.status === "completed" ? <WorkspaceIcon name="check" width="12" height="12" /> : event.data.status === "failed" ? "!" : event.data.status === "running" && running ? <WorkspaceIcon name="activity" width="12" height="12" /> : "–"}</span>
            <span>{operatorToolLabel(event.data.name)}{event.data.status === "failed" ? <small> · Failed</small> : null}</span>
          </div>)}
        </div></div></div>
      </div>
      <ResponseText streaming={running} text={timeline.preamble?.data.content} />
      <ResponseText streaming={running} text={answer?.content ?? timeline.tail?.data.content} />
      {webSources.length ? <OperatorSources sources={webSources} /> : null}
      {inline}
      {surfaces.length ? <div className={styles.surfaces} aria-label="Results">{surfaces.map(surface => <button key={surfaceKey(surface)} onClick={() => onSelect(surface)}><WorkspaceIcon name={surface.kind === "file" || surface.kind === "repository-changes" ? "file" : "panel"} width="15" height="15" />{surface.path ?? surface.system ?? surfaceLabels[surface.kind]}</button>)}</div> : null}
    </div>
  </section>;
}

// Keep a substantive activity readable, coalescing bursts to the newest phase.
function useActivityLabel(next: string, running: boolean) {
  const [label, setLabel] = useState(next);
  const displayedAt = useRef(0);
  useEffect(() => {
    if (!displayedAt.current) displayedAt.current = performance.now();
    if (label === next) return;
    const delay = running ? Math.max(0, 900 - (performance.now() - displayedAt.current)) : 0;
    const timer = setTimeout(() => { displayedAt.current = performance.now(); setLabel(next); }, delay);
    return () => clearTimeout(timer);
  }, [label, next, running]);
  return label;
}
