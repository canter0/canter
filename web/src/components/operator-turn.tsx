"use client";
import { useEffect, useState, type ReactNode } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { elapsedLabel, turnTimeline } from "@/lib/operator-timeline";
import { isSurface, surfaceKey, surfaceLabels, type OperatorEvent, type OperatorMessage, type OperatorSurface } from "@/lib/operator-api";
import { OperatorAttachments } from "./operator-attachments";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./operator-workspace.module.css";

const toolLabels: Record<string, string> = {
  canter_show_repositories: "Opening GitHub repositories", canter_show_repository_changes: "Reading code changes",
  canter_show_apps: "Reading apps", canter_show_deployments: "Reading deployments", canter_show_billing: "Reading billing", canter_show_activity: "Reading activity", canter_show_agents: "Reading agent access",
  canter_inspect_repository: "Inspecting repository", canter_read_repository_file: "Reading source", canter_prepare_repository_deployment: "Preparing deployment", canter_capabilities: "Checking capabilities",
  canter_inspect_system: "Inspecting app", canter_inspect_initial_deployment: "Reading deployment", canter_inspect_initial_deployment_execution: "Reading execution", canter_inspect_change: "Reading change", canter_draft_change: "Preparing change", canter_list_changes: "Reading changes", canter_list_standing_policies: "Checking policies", canter_apply_change_under_policy: "Applying authorized change",
};
function ResponseText({ text }: { text: unknown }) {
  if (typeof text !== "string" || !text) return null;
  return <div className={styles.markdown}><Markdown remarkPlugins={[remarkGfm]} skipHtml disallowedElements={["img", "hr"]} components={{ a: ({ href, children }) => <a href={href} target="_blank" rel="noopener noreferrer">{children}</a> }}>{text}</Markdown></div>;
}
export function OperatorTurn({ message, answer, events, running, onSelect, inline }: { message: OperatorMessage; answer?: OperatorMessage; events: OperatorEvent[]; running: boolean; onSelect: (surface: OperatorSurface) => void; inline?: ReactNode }) {
  const [expanded, setExpanded] = useState<boolean | null>(null);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => { if (!running) return; const timer = setInterval(() => setNow(Date.now()), 1000); return () => clearInterval(timer); }, [running]);
  const timeline = turnTimeline(events);
  const finished = events.find(event => event.kind === "finished");
  const elapsed = elapsedLabel(events[0]?.createdAt ?? message.createdAt, finished?.createdAt ?? (running ? now : events.at(-1)?.createdAt ?? message.createdAt));
  const surfaces = [...new Map(events.filter(event => event.kind === "surface" && isSurface(event.data) && event.data.kind !== "github").map(event => [surfaceKey(event.data as OperatorSurface), event.data as OperatorSurface])).values()];
  const open = expanded ?? running;
  return <section className={styles.turn} aria-label="Conversation turn">
    <article className={styles.message} data-role="user"><div className={styles.messageText}>{message.content}</div>{message.attachments?.length ? <OperatorAttachments items={message.attachments} /> : null}</article>
    <div className={styles.agentTurn}>
      <ResponseText text={timeline.preamble?.data.content} />
      {timeline.work.length ? <div className={styles.operations}>
        <button className={styles.workToggle} aria-expanded={open} onClick={() => setExpanded(!open)}><WorkspaceIcon name={open ? "down" : "chevron"} width="14" height="14" />{running ? "Working" : "Worked"}{elapsed ? ` for ${elapsed}` : ""}</button>
        {open ? <div className={styles.workLog}>{timeline.work.map(event => event.kind === "text" ? <ResponseText key={event.sequence} text={event.data.content} /> : <div key={String(event.data.callId)} className={styles.toolRow}><span className={event.data.status === "running" && running ? styles.pulse : styles.toolIcon}>{event.data.status === "completed" ? <WorkspaceIcon name="check" width="12" height="12" /> : event.data.status === "failed" ? "!" : event.data.status === "running" && !running ? "–" : null}</span><span>{toolLabels[String(event.data.name)] ?? String(event.data.name).replace(/^canter_/, "").replaceAll("_", " ")}{event.data.status === "failed" ? <small> · Failed</small> : null}</span></div>)}</div> : null}
      </div> : null}
      <ResponseText text={answer?.content ?? timeline.tail?.data.content} />
      {running ? <div className={styles.progress} role="status"><span className={styles.pulse} />{timeline.work.length ? "Working in your workspace…" : "Canter is working…"}</div> : null}
      {inline}
      {surfaces.length ? <div className={styles.surfaces} aria-label="Results">{surfaces.map(surface => <button key={surfaceKey(surface)} onClick={() => onSelect(surface)}><WorkspaceIcon name={surface.kind === "file" || surface.kind === "repository-changes" ? "file" : "panel"} width="15" height="15" />{surface.path ?? surface.system ?? surfaceLabels[surface.kind]}</button>)}</div> : null}
    </div>
  </section>;
}
