"use client";
import { useEffect, useRef, useState, type ReactNode } from "react";
import Link from "next/link";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { operatorWebSources } from "@/lib/operator-web-sources";
import { elapsedLabel, turnTimeline } from "@/lib/operator-timeline";
import { isSurface, surfaceKey, surfaceLabels, type Conversation, type OperatorEvent, type OperatorMessage, type OperatorSurface } from "@/lib/operator-api";
import { OperatorAttachments } from "./operator-attachments";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./operator-workspace.module.css";

const toolLabels: Record<string, string> = {
  canter_search_web: "Searching the web", canter_open_web: "Opening a web source", canter_read_web: "Reading saved sources",
  canter_bash: "Working with workspace files", canter_save_context: "Saving project context", canter_search_history: "Finding earlier decisions", canter_read_history: "Reading conversation context", canter_read_result: "Reading saved results", canter_create_task: "Queuing agent task", canter_list_tasks: "Reading tasks", canter_inspect_task: "Checking task progress", canter_read_task_context: "Reading task context",
  canter_show_compute: "Preparing compute plan", canter_estimate_compute_cost: "Calculating Canter compute estimate", canter_show_storage: "Checking storage capabilities",
  canter_show_repositories: "Opening GitHub repositories", canter_show_repository_changes: "Reading code changes",
  canter_show_apps: "Reading apps", canter_show_deployments: "Reading deployments", canter_show_billing: "Reading billing", canter_show_activity: "Reading activity", canter_show_agents: "Reading agent access",
  canter_inspect_repository: "Inspecting repository", canter_read_repository_file: "Reading source", canter_prepare_repository_deployment: "Preparing deployment", canter_capabilities: "Checking capabilities",
  canter_inspect_system: "Inspecting app", canter_inspect_initial_deployment: "Reading deployment", canter_inspect_initial_deployment_execution: "Reading execution", canter_inspect_change: "Reading change", canter_draft_change: "Preparing change", canter_list_changes: "Reading changes", canter_list_standing_policies: "Checking policies", canter_apply_change_under_policy: "Applying authorized change",
};
function ResponseText({ text, streaming = false }: { text: unknown; streaming?: boolean }) {
  const content = typeof text === "string" ? text : "";
  const [visible, setVisible] = useState(streaming ? "" : content);
  const shown = useRef(streaming ? "" : content);
  const animated = useRef(streaming);
  useEffect(() => {
    let frame = 0;
    let previous = 0;
    animated.current ||= streaming;
    const reduced = window.matchMedia("(prefers-reduced-motion: reduce)");
    const tick = (time: number) => {
      if (!animated.current || reduced.matches || !content.startsWith(shown.current)) {
        shown.current = content;
      } else {
        const elapsed = previous ? Math.min(time - previous, 64) : 16;
        const remaining = content.length - shown.current.length;
        // Catch up to bursty snapshots in about 180ms, without delaying long answers.
        let end = Math.min(content.length, shown.current.length + Math.max(1, Math.ceil(remaining * elapsed / 180)));
        if (end < content.length && /[\uD800-\uDBFF]/.test(content[end - 1])) end++;
        shown.current = content.slice(0, end);
      }
      previous = time;
      setVisible(shown.current);
      if (shown.current !== content) frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
  }, [content, streaming]);
  if (!visible) return null;
  return <div className={styles.markdown} data-streaming={streaming}><Markdown remarkPlugins={[remarkGfm]} skipHtml disallowedElements={["img", "hr"]} components={{ a: ({ href, children }) => <a href={href} target="_blank" rel="noopener noreferrer">{children}</a> }}>{visible}</Markdown></div>;
}
export function OperatorMessageContext({ surface, conversations }: { surface?: OperatorSurface | null; conversations: Conversation[] }) {
  if (!surface) return null;
  const title = surface.kind === "conversation" ? conversations.find(item => item.id === surface.id)?.title ?? "Earlier conversation" : surface.path?.split("/").at(-1) ?? surface.repository ?? surface.system ?? (surface.kind === "github" ? "GitHub repositories" : surfaceLabels[surface.kind]);
  const content = <><WorkspaceIcon name={surface.kind === "conversation" ? "message" : surface.kind === "repository" || surface.kind === "github" ? "folder" : surface.kind === "apps" || surface.kind === "app" ? "apps" : "file"} width="13" height="13" /><span>Context</span><strong title={title}>{title}</strong></>;
  return surface.kind === "conversation" && surface.id ? <Link className={styles.messageContext} href={`/app/conversations/${encodeURIComponent(surface.id)}`} aria-label={`Open referenced conversation: ${title}`}>{content}</Link> : <span className={styles.messageContext} aria-label={`Context: ${title}`}>{content}</span>;
}
export function OperatorTurn({ message, answer, events, running, onSelect, conversations, inline }: { message: OperatorMessage; answer?: OperatorMessage; events: OperatorEvent[]; running: boolean; onSelect: (surface: OperatorSurface) => void; conversations: Conversation[]; inline?: ReactNode }) {
  const [expanded, setExpanded] = useState<boolean | null>(null);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => { if (!running) return; const timer = setInterval(() => setNow(Date.now()), 1000); return () => clearInterval(timer); }, [running]);
  const timeline = turnTimeline(events);
  const webSources = operatorWebSources(events);
  const finished = events.find(event => event.kind === "finished");
  const elapsed = elapsedLabel(events[0]?.createdAt ?? message.createdAt, finished?.createdAt ?? (running ? now : events.at(-1)?.createdAt ?? message.createdAt));
  const surfaces = [...new Map(events.filter(event => event.kind === "surface" && isSurface(event.data) && !["github", "compute", "storage"].includes(String(event.data.kind))).map(event => [surfaceKey(event.data as OperatorSurface), event.data as OperatorSurface])).values()];
  const open = expanded ?? running;
  return <section className={styles.turn} aria-label="Conversation turn">
    <article className={styles.message} data-role="user"><div className={styles.messageText}>{message.content}</div><OperatorMessageContext surface={message.surface} conversations={conversations} />{message.attachments?.length ? <OperatorAttachments items={message.attachments} /> : null}</article>
    <div className={styles.agentTurn}>
      <ResponseText streaming={running} text={timeline.preamble?.data.content} />
      {timeline.work.length ? <div className={styles.operations} data-streaming={running}>
        <button className={styles.workToggle} aria-expanded={open} onClick={() => setExpanded(!open)}><WorkspaceIcon className={styles.workChevron} name="chevron" width="14" height="14" />{running ? "Working" : "Worked"}{elapsed ? ` for ${elapsed}` : ""}</button>
        <div className={styles.workLogReveal} data-open={open} aria-hidden={!open}><div className={styles.workLogClip}><div className={styles.workLog}>{timeline.work.map(event => event.kind === "text" ? <ResponseText streaming={running} key={event.sequence} text={event.data.content} /> : <div key={String(event.data.callId)} className={styles.toolRow}><span className={event.data.status === "running" && running ? styles.pulse : styles.toolIcon}>{event.data.status === "completed" ? <WorkspaceIcon name="check" width="12" height="12" /> : event.data.status === "failed" ? "!" : event.data.status === "running" && !running ? "–" : null}</span><span>{toolLabels[String(event.data.name)] ?? String(event.data.name).replace(/^canter_/, "").replaceAll("_", " ")}{event.data.status === "failed" ? <small> · Failed</small> : null}</span></div>)}</div></div></div>
      </div> : null}
      <ResponseText streaming={running} text={answer?.content ?? timeline.tail?.data.content} />
      {running ? <div className={styles.progress} role="status"><span className={styles.pulse} />{timeline.work.length ? "Working in your workspace…" : "Canter is working…"}</div> : null}
      {webSources.length ? <details className={styles.webSources}><summary>{webSources.length} {webSources.length === 1 ? "source" : "sources"} consulted</summary><ul>{webSources.map(source => <li key={source.url}><a href={source.url} target="_blank" rel="noopener noreferrer" title={`Retrieved ${source.retrievedAt}`}>{source.title}</a><span>{new URL(source.url).hostname}</span></li>)}</ul></details> : null}
      {inline}
      {surfaces.length ? <div className={styles.surfaces} aria-label="Results">{surfaces.map(surface => <button key={surfaceKey(surface)} onClick={() => onSelect(surface)}><WorkspaceIcon name={surface.kind === "file" || surface.kind === "repository-changes" ? "file" : "panel"} width="15" height="15" />{surface.path ?? surface.system ?? surfaceLabels[surface.kind]}</button>)}</div> : null}
    </div>
  </section>;
}
