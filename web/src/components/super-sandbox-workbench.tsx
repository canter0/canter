"use client";

import Link from "next/link";
import { Progress, ProgressIndicator } from "@ui-sdk/react/progress";
import { runtimeAllocation, superSandboxProgress, superSandboxStateLabel, superSandboxTerminal, workerDuration, type SuperSandbox } from "@/lib/super-sandboxes";
import { useWrenResource, workBase } from "@/lib/wren-work";
import { useWorkspace } from "./workspace-context";
import { SuperSandboxRequest } from "./super-sandbox-dashboard";
import styles from "./super-sandbox.module.css";
import shared from "./workspace.module.css";
import consoleStyles from "./sandbox-console.module.css";

const alwaysLive = () => true;
type Props = { workspaceId: string; id: string; onRequest?: (prompt: string) => void };

export function SuperSandboxWorkbench(props: Props) {
  const { data } = useWorkspace();
  return <SuperSandboxWorkbenchInner key={`${data?.account.id}:${props.workspaceId}:${props.id}`} {...props} />;
}

function SuperSandboxWorkbenchInner({ workspaceId, id, onRequest }: Props) {
  const resource = useWrenResource<SuperSandbox>(`${workBase(workspaceId)}/super-sandboxes/${encodeURIComponent(id)}`, 4000, alwaysLive);
  const run = resource.data;
  const progress = run ? superSandboxProgress(run.tasks) : null;
  return <section className={styles.root} aria-label="Super Sandbox details">
    <Link className={styles.back} href="/app/sandboxes/super">← Super Sandboxes</Link>
    <header className={consoleStyles.workHeader}>
      <div className={consoleStyles.workTitle}><h1>{run?.name ?? "Super Sandbox"}</h1>
        <p className={consoleStyles.workMeta}>
          {run ? <span className={shared.status} data-phase={run.state === "needs_attention" ? "escalated" : run.state === "queued" ? "drafted" : run.state === "cancelling" ? "running" : run.state}>{superSandboxStateLabel(run.state)}</span> : null}
          <code className={consoleStyles.workId}>{id}</code>
        </p>
      </div>
      <div className={consoleStyles.workActions}><Link href="/app" className={shared.primaryButton}>Ask Canter</Link></div>
    </header>
    {resource.error ? <p role="alert" className={shared.error}>{run ? "Couldn’t refresh. Showing the last loaded batch. " : ""}{resource.error} <button type="button" onClick={resource.refresh}>Retry</button></p> : null}
    {!run && !resource.error ? <p role="status" className={shared.loading}>Loading Super Sandbox…</p> : null}
    {run && progress ? <>
      <dl className={consoleStyles.facts}>
        <div><dt>Completed / total</dt><dd>{progress.completed} / {progress.total} <small>· {progress.succeeded} succeeded</small></dd></div>
        <div><dt>Workers / concurrency</dt><dd>{run.workerIds.length} / {run.maxConcurrency}</dd></div>
        <div><dt>Runtime reserved / cap</dt><dd>{runtimeAllocation(run)}</dd></div>
      </dl>
      {progress.total ? <Progress className={styles.progress} value={progress.completed} max={progress.total} aria-label={`${progress.completed} of ${progress.total} tasks completed`}><ProgressIndicator className={styles.progressIndicator} /></Progress> : null}
      {run.state === "needs_attention" || progress.unknown > 0 ? <p role="status" className={`${shared.status} ${styles.notice}`} data-phase="escalated">Needs attention. {progress.unknown ? `${progress.unknown} task outcome${progress.unknown === 1 ? " is" : "s are"} unknown and not counted as completed. ` : ""}Inspect the worker jobs before deciding what to do next.</p> : null}
      {run.failure ? <p className={`${shared.status} ${styles.notice}`} data-phase="failed">{run.failure}</p> : null}
      <p className={styles.note}>Runtime reserves the worst-case sum of task timeouts upfront, not actual usage or a money cap. Existing worker VM billing continues separately.</p>
      <h2 className={styles.sectionTitle}>Tasks</h2>
      {run.tasks.length ? <div className={styles.tableScroll} role="region" aria-label="Tasks" tabIndex={0}>
        <table className={`${styles.table} ${styles.taskTable}`} role="table">
          <thead><tr><th scope="col">Task / command</th><th scope="col">State</th><th scope="col">Worker</th><th scope="col">Exit / result</th><th scope="col">Timeout</th></tr></thead>
          <tbody>{[...run.tasks].sort((a, b) => a.index - b.index).map(task => <tr key={task.id}>
            <th scope="row">{task.name}<code className={styles.command}>{task.command}</code>{task.directory ? <small className={styles.identifier}>{task.directory}</small> : null}</th>
            <td><span className={shared.status} data-phase={task.state === "unknown" ? "escalated" : task.state === "starting" ? "running" : task.state === "queued" ? "drafted" : task.state}>{superSandboxStateLabel(task.state)}</span></td>
            <td data-label="Worker">{task.workerId ? <Link className={`${styles.resourceLink} ${styles.code}`} href={`/app/sandboxes/${encodeURIComponent(task.workerId)}`}>{task.workerId}</Link> : "Not assigned"}</td>
            <td data-label="Exit / result">{task.exitCode !== undefined ? `Exit ${task.exitCode}` : task.state === "unknown" ? "Outcome unknown" : task.state === "cancelled" ? "Cancelled" : "No exit reported"}
              {task.jobId ? <small className={styles.identifier}>Job {task.jobId}</small> : null}
              {task.error ? <small className={`${shared.status} ${styles.annotation}`} data-phase={task.state === "unknown" ? "escalated" : "failed"}>{task.error}</small> : null}</td>
            <td data-label="Timeout">{workerDuration(task.timeoutSeconds)}</td>
          </tr>)}</tbody>
        </table>
      </div> : <p className={styles.note}>No tasks recorded.</p>}
      <h2 className={styles.sectionTitle}>Workers</h2>
      <ul className={styles.workers}>{run.workerIds.map(workerId => <li key={workerId}><Link className={`${styles.resourceLink} ${styles.code}`} href={`/app/sandboxes/${encodeURIComponent(workerId)}`}>{workerId}</Link></li>)}</ul>
      <p className={styles.note}>Open a worker’s Jobs section with a job ID to read logs and results. Worker files remain local. RAM is not pooled.</p>
      <dl className={styles.timestamps}>
        <div><dt>Created</dt><dd><Timestamp value={run.createdAt} /></dd></div>
        {run.startedAt ? <div><dt>Started</dt><dd><Timestamp value={run.startedAt} /></dd></div> : null}
        {run.finishedAt ? <div><dt>Finished</dt><dd><Timestamp value={run.finishedAt} /></dd></div> : null}
      </dl>
      {!superSandboxTerminal(run.state) ? <div className={styles.actions}><SuperSandboxRequest workspaceId={workspaceId} id={id} onRequest={onRequest} /></div> : null}
    </> : null}
  </section>;
}

function Timestamp({ value }: { value: string }) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? <>{value}</> : <time dateTime={value}>{date.toLocaleString()}</time>;
}
