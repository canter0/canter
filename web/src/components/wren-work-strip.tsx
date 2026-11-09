"use client";
import { useEffect, useRef, type RefObject } from "react";
import type { OperatorEvent, OperatorSurface } from "@/lib/operator-api";
import { activeJob, useWrenResource, workBase, workUpdateEvent, type WrenJobRecord } from "@/lib/wren-work";
import { memoryForgetNotices, notifyMemoryForgotten } from "@/lib/wren-knowledge";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./wren-work-strip.module.css";
const jobsActive = (value: { jobs: WrenJobRecord[] }) => value.jobs.some(activeJob);
export function WrenWorkStrip({ workspaceId, conversationId, events, onSelect, triggerRef, onBrowse, onChat, onConfigure, workOpen, membersLabel, computerOpen = false }: { membersLabel?:string; onBrowse:()=>void; onChat:()=>void; onConfigure?:()=>void; workOpen?:boolean; triggerRef?: RefObject<HTMLButtonElement | null>; computerOpen?: boolean; workspaceId: string; conversationId: string; events: OperatorEvent[]; onSelect: (surface: OperatorSurface, label?: string) => void }) {
  const base = workBase(workspaceId), query = `?conversationId=${encodeURIComponent(conversationId)}`;
  const jobs = useWrenResource<{ jobs: WrenJobRecord[] }>(`${base}/wren-jobs${query}`, 5000, jobsActive);
  const eventRevision = events.filter(event => ["job", "job_result", "artifact", "computer", "surface", "responsibility", "memory", "skill"].includes(event.kind)).at(-1)?.sequence ?? 0;
  const refreshJobs = jobs.refresh;
  const knowledgeDelivered = useRef({ scope: "", sequence: 0 });
  useEffect(() => {
    const scope = `${workspaceId}:${conversationId}`;
    if (knowledgeDelivered.current.scope !== scope) knowledgeDelivered.current = { scope, sequence: 0 };
    const fresh = events.filter(event => event.sequence > knowledgeDelivered.current.sequence && ["memory", "skill"].includes(event.kind));
    if (!fresh.length) return;
    for (const notice of memoryForgetNotices(fresh)) notifyMemoryForgotten(workspaceId, conversationId, notice.id);
    knowledgeDelivered.current.sequence = fresh.at(-1)!.sequence;
    window.dispatchEvent(new CustomEvent(workUpdateEvent, { detail: { workspaceId, conversationId, reason: "knowledge-event" } }));
  }, [workspaceId, conversationId, events]);

  useEffect(() => { if (eventRevision) refreshJobs(); }, [eventRevision, refreshJobs]);
  const active = jobs.data?.jobs.filter(activeJob) ?? [];
  return <div className={styles.strip} aria-label="Conversation navigation">
    {membersLabel ? <button type="button" className={styles.members} aria-label={`Conversation members: ${membersLabel}`} onClick={onConfigure}><WorkspaceIcon name="profile" width="16" height="16"/><span>{membersLabel}</span></button> : null}
    <button type="button" aria-label={workOpen ? "Back to chat" : "Open conversation work"} aria-pressed={!!workOpen} onClick={workOpen ? onChat : onBrowse}><WorkspaceIcon name={workOpen ? "message" : "folder"} width="17" height="17"/><span>{workOpen ? "Chat" : "Work"}</span>{active.length ? <span className={styles.workCount}>{active.length}</span> : null}</button>
    <button ref={triggerRef} type="button" aria-label={computerOpen ? "Close computer panel" : "Open computer"} title="Computer" aria-expanded={computerOpen} onClick={()=>onSelect({kind:"computer"})}><WorkspaceIcon name="panel" width="18" height="18"/></button>
    {onConfigure ? <button type="button" aria-label="Conversation details" title="Conversation details" onClick={onConfigure}><WorkspaceIcon name="more" width="19" height="19"/></button> : null}
  </div>;
}
