"use client";

import { ConnectAgentButton } from "@/components/connect-agent-button";

import Link from "next/link";
import { useParams } from "next/navigation";
import { useEffect, useState } from "react";
import { AppShell } from "@/components/app-shell";
import { ActionFeed } from "@/components/action-feed";
import { useWorkspace } from "@/components/workspace-context";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { canterFetch, type WorkspaceTask } from "@/lib/canter-api";
import { taskModels, taskStatus } from "@/lib/task-options";
import styles from "@/components/workspace.module.css";

export default function TaskPage() {
  const { id } = useParams<{ id: string }>();
  const { data, error: workspaceError } = useWorkspace();
  const [detail, setDetail] = useState<WorkspaceTask | null>(null);
  const [error, setError] = useState("");
  const workspaceID = data?.workspace.id;
  useEffect(() => {
    if (!workspaceID) return;
    const controller = new AbortController();
    canterFetch<WorkspaceTask>(`/workspaces/${encodeURIComponent(workspaceID)}/tasks/${encodeURIComponent(id)}`, { signal: controller.signal }).then(task => { if (!controller.signal.aborted) { setDetail(task); setError(""); } }).catch(cause => { if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Could not load task."); });
    return () => controller.abort();
  }, [workspaceID, id]);
  const task = detail?.id === id ? { ...detail, ...data?.tasks.find(item => item.id === id) } : null;
  const agent = data?.installations.find(item => item.id === (task?.claimedBy || task?.targetInstallationId));
  const actions = data?.actions.filter(action => action.subject === id || action.metadata.taskId === id) ?? [];

  return <AppShell active="Task"><section className={styles.taskPage}>
    {error || workspaceError ? <p role="alert" className={styles.error}>{error || workspaceError}</p> : !task ? <p role="status" className={styles.loading}>Loading task…</p> : <>
      <div className={styles.taskHeading}><span className={styles.status} data-phase={task.status}><span className={styles.connectionDot} data-connected={task.status === "working" || task.status === "completed"} />{taskStatus[task.status]}</span><Link href="/app" className={styles.secondaryButton}><WorkspaceIcon name="plus" width="15" height="15" />New task</Link></div>
      <h1 className={styles.taskPrompt}>{task.prompt}</h1>
      <div className={styles.taskMeta}><span title="Requested model">{taskModels.find(model => model.id === task.model)?.label ?? task.model}</span><span>{task.reasoning} reasoning</span>{agent ? <Link href={`/app/agents/${encodeURIComponent(agent.id)}`}>{agent.name}</Link> : null}</div>
      {task.context?.length ? <div className={styles.taskContext}>{task.context.map(item => {
        const content = <><WorkspaceIcon name={item.kind === "repository" ? "folder" : item.kind === "app" ? "apps" : item.kind === "task" ? "message" : "file"} width="15" height="15" /><span>{item.name}</span><WorkspaceIcon name="external" width="13" height="13" /></>;
        const href = item.kind === "attachment" ? `/api/canter/workspaces/${encodeURIComponent(task.workspaceId)}/tasks/${encodeURIComponent(task.id)}/context/${encodeURIComponent(item.id)}` : item.kind === "repository" ? item.url : item.kind === "app" ? `/app/system/${encodeURIComponent(item.referenceId ?? "")}` : `/app/tasks/${encodeURIComponent(item.referenceId ?? "")}`;
        return <a key={item.id} href={href} className={styles.contextChip} target={item.kind === "repository" ? "_blank" : undefined} rel={item.kind === "repository" ? "noopener noreferrer" : undefined}>{content}</a>;
      })}</div> : null}
      {task.status === "queued" ? <div className={styles.waitingPanel}><WorkspaceIcon name="terminal" width="20" height="20" /><div><h2>{agent ? `Waiting for ${agent.name}` : "Waiting for an agent"}</h2><p>Your request is saved. A connected agent can pick it up from Canter.</p></div>{agent ? <Link href={`/app/agents/${encodeURIComponent(agent.id)}`}>View agent<WorkspaceIcon name="external" width="14" height="14" /></Link> : <ConnectAgentButton>Bring your agent<WorkspaceIcon name="external" width="14" height="14" /></ConnectAgentButton>}</div> : null}
      {task.result ? <div className={styles.taskResult}><div className={styles.sectionHeading}><h2>{agent?.name ?? "Agent"}’s result</h2></div><p>{task.result}</p></div> : null}
      <div className={styles.sectionHeading}><h2>Activity</h2><span className={styles.refreshLabel}>Updates automatically</span></div>
      <ActionFeed actions={actions} />
      {!actions.length ? <p className={styles.pickerNote}>No recorded actions yet.</p> : null}
    </>}
  </section></AppShell>;
}
