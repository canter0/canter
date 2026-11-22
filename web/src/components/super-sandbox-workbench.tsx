"use client";

import Link from "next/link";
import { useCallback, useRef, useState, type FormEvent } from "react";
import { Progress, ProgressIndicator } from "@ui-sdk/react/progress";
import { canterFetch } from "@/lib/canter-api";
import { runtimeAllocation, superSandboxBlockerLabel, superSandboxProgress, superSandboxStateLabel, superSandboxTerminal, workerDuration, type SuperSandbox } from "@/lib/super-sandboxes";
import { useWrenResource, workBase } from "@/lib/wren-work";
import { useDialog } from "./use-dialog";
import { useRequestScope } from "./use-request-scope";
import { useWorkspace } from "./workspace-context";
import dialogStyles from "./agent-connection-dialog.module.css";
import styles from "./super-sandbox.module.css";
import shared from "./workspace.module.css";
import consoleStyles from "./sandbox-console.module.css";

const isActive = (run: SuperSandbox) => !superSandboxTerminal(run.state);
type Props = { workspaceId: string; id: string; onRequest?: (prompt: string) => void };

export function SuperSandboxWorkbench(props: Props) {
  const { data } = useWorkspace();
  return <SuperSandboxWorkbenchInner key={`${data?.account.id}:${data?.workspace.id}:${data?.workspace.role}:${props.workspaceId}:${props.id}`} {...props} />;
}

// CancelSuperSandboxDialog is a direct stop control, not a chat handoff. The
// scope captured at open stays pinned while the dialog is mounted; on scope
// change or role loss the caller unmounts it.
function CancelSuperSandboxDialog({ workspaceId, id, onCancelled, onClose }: {
  workspaceId: string;
  id: string;
  onCancelled: (run: SuperSandbox) => void;
  onClose: () => void;
}) {
  const { signal } = useRequestScope();
  const ref = useRef<HTMLDialogElement>(null);
  const dismiss = useDialog(ref, "[data-keep-running]");
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const pendingRef = useRef(false);

  const close = useCallback(() => { if (!pendingRef.current) dismiss(onClose); }, [dismiss, onClose]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    // One request per confirmation; a double submission must not race.
    if (pendingRef.current) return;
    const requestSignal = signal();
    if (requestSignal.aborted) return;
    pendingRef.current = true;
    setPending(true);
    setError("");
    try {
      const run = await canterFetch<SuperSandbox>(`${workBase(workspaceId)}/super-sandboxes/${encodeURIComponent(id)}/cancel`, { method: "POST", signal: requestSignal });
      // Render the returned state, never an optimistic "Cancelled".
      if (!requestSignal.aborted) onCancelled(run);
    } catch (cause) {
      if (!requestSignal.aborted) setError(cause instanceof Error ? cause.message : "Couldn't request cancellation.");
    } finally {
      pendingRef.current = false;
      if (!requestSignal.aborted) setPending(false);
    }
  }

  return <dialog ref={ref} className={dialogStyles.dialog} aria-labelledby="cancel-super-sandbox-title" onCancel={event => { event.preventDefault(); close(); }}>
    <button className={dialogStyles.close} aria-label="Close" disabled={pending} onClick={close}>✕</button>
    <h2 id="cancel-super-sandbox-title">Cancel batch {id}?</h2>
    <p>Queued tasks won’t start. Canter will try to stop this batch’s running jobs. This won’t undo changes, stop workers, or stop VM billing.</p>
    <form onSubmit={submit}>
      {error ? <p className={dialogStyles.error} role="alert">{error}</p> : null}
      <button type="button" className={shared.secondaryButton} data-keep-running disabled={pending} onClick={close}>Keep running</button>
      <button className={dialogStyles.primary} type="submit" disabled={pending}>{pending ? "Cancelling…" : "Cancel batch"}</button>
    </form>
  </dialog>;
}

function SuperSandboxWorkbenchInner({ workspaceId, id, onRequest }: Props) {
  const { data } = useWorkspace();
  const workspaceMatch = data?.workspace.id === workspaceId;
  const canCancel = workspaceMatch && ["owner", "operator"].includes(data?.workspace.role ?? "");
  const resource = useWrenResource<SuperSandbox>(`${workBase(workspaceId)}/super-sandboxes/${encodeURIComponent(id)}`, 4000, isActive);
  const run = resource.data;
  const [confirming, setConfirming] = useState(false);
  const [ack, setAck] = useState<{ baseline: SuperSandbox | undefined; response: SuperSandbox } | null>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  // Show the POST response only until the resource refreshes past the
  // baseline it was returned against; never an optimistic final state.
  const shown = ack && run === ack.baseline ? ack.response : run;
  const progress = shown ? superSandboxProgress(shown.tasks) : null;
  const blocker = superSandboxBlockerLabel(shown?.blockedReason);
  return <section className={styles.root} aria-label="Super Sandbox details">
    <Link className={styles.back} href="/app/sandboxes/super">← Super Sandboxes</Link>
    <header className={consoleStyles.workHeader}>
      <div className={consoleStyles.workTitle}><h1 ref={heading} tabIndex={-1}>{shown?.name ?? "Super Sandbox"}</h1>
        <p className={consoleStyles.workMeta}>
          {shown ? <span className={shared.status} data-phase={shown.state === "needs_attention" ? "escalated" : shown.state === "queued" ? "drafted" : shown.state === "cancelling" ? "running" : shown.state}>{superSandboxStateLabel(shown.state)}</span> : null}
          <code className={consoleStyles.workId}>{id}</code>
        </p>
      </div>
      <div className={consoleStyles.workActions}>
        {canCancel && shown && isActive(shown) && shown.state !== "cancelling" ? <button type="button" className={shared.secondaryButton} onClick={() => setConfirming(true)}>Cancel batch</button> : null}
        <Link href="/app" className={shared.primaryButton}>Ask Canter</Link>
      </div>
    </header>
    {resource.error ? <p role="alert" className={shared.error}>{run ? "Couldn’t refresh. Showing the last loaded batch. " : ""}{resource.error} <button type="button" onClick={resource.refresh}>Retry</button></p> : null}
    {!run && !resource.error ? <p role="status" className={shared.loading}>Loading Super Sandbox…</p> : null}
    {shown && progress ? <>
      {blocker ? <p role="status" className={`${shared.status} ${styles.notice}`} data-phase="escalated">{blocker}</p> : null}
      {shown.state === "needs_attention" && !blocker ? <p role="status" className={`${shared.status} ${styles.notice}`} data-phase="escalated">Needs attention. {progress.unknown ? `${progress.unknown} task outcome${progress.unknown === 1 ? " is" : "s are"} unknown and not counted as settled. ` : ""}Inspect the worker jobs before deciding what to do next.</p> : null}
      {shown.failure ? <p className={`${shared.status} ${styles.notice}`} data-phase="failed">{shown.failure}</p> : null}
      <dl className={consoleStyles.facts}>
        <div><dt>Settled / total</dt><dd>{progress.settled} / {progress.total} <small>· {progress.succeeded} succeeded</small></dd></div>
        <div><dt>Workers / concurrency</dt><dd>{shown.workerIds.length} / {shown.maxConcurrency}</dd></div>
        <div><dt>Runtime reserved / cap</dt><dd>{runtimeAllocation(shown)}</dd></div>
      </dl>
      {progress.total ? <Progress className={styles.progress} value={progress.settled} max={progress.total} aria-label={`${progress.settled} of ${progress.total} tasks settled`}><ProgressIndicator className={styles.progressIndicator} /></Progress> : null}
      <p className={styles.note}>Runtime reserves the worst-case sum of task timeouts upfront, not actual usage or a money cap. Existing worker VM billing continues separately.</p>
      <h2 className={styles.sectionTitle}>Tasks</h2>
      {shown.tasks.length ? <div className={styles.tableScroll} role="region" aria-label="Tasks" tabIndex={0}>
        <table className={`${styles.table} ${styles.taskTable}`} role="table">
          <thead><tr><th scope="col">Task / command</th><th scope="col">State</th><th scope="col">Worker</th><th scope="col">Exit / result</th><th scope="col">Timeout</th></tr></thead>
          <tbody>{[...shown.tasks].sort((a, b) => a.index - b.index).map(task => <tr key={task.id}>
            <th scope="row">{task.name}<code className={styles.command}>{task.command}</code>{task.directory ? <small className={styles.identifier}>{task.directory}</small> : null}</th>
            <td><span className={shared.status} data-phase={task.state === "unknown" ? "escalated" : task.state === "outcome_unavailable" ? "failed" : task.state === "starting" ? "running" : task.state === "queued" ? "drafted" : task.state}>{superSandboxStateLabel(task.state)}</span></td>
            <td data-label="Worker">{task.workerId ? <Link className={`${styles.resourceLink} ${styles.code}`} href={`/app/sandboxes/${encodeURIComponent(task.workerId)}`}>{task.workerId}</Link> : "Not assigned"}</td>
            <td data-label="Exit / result">{task.exitCode !== undefined ? `Exit ${task.exitCode}` : task.state === "unknown" ? "Outcome unknown" : task.state === "outcome_unavailable" ? "Outcome unavailable" : task.state === "cancelled" ? "Cancelled" : "No exit reported"}
              {task.jobId ? <small className={styles.identifier}>Job {task.jobId} <CopyJobId jobId={task.jobId} /></small> : null}
              {task.state === "outcome_unavailable" ? <small className={`${shared.status} ${styles.annotation}`} data-phase="failed">A live log may no longer be available after worker removal.</small> : null}
              {task.error ? <small className={`${shared.status} ${styles.annotation}`} data-phase={task.state === "unknown" ? "escalated" : "failed"}>{task.error}</small> : null}</td>
            <td data-label="Timeout">{workerDuration(task.timeoutSeconds)}</td>
          </tr>)}</tbody>
        </table>
      </div> : <p className={styles.note}>No tasks recorded.</p>}
      <h2 className={styles.sectionTitle}>Workers</h2>
      <ul className={styles.workers}>{shown.workerIds.map(workerId => <li key={workerId}><Link className={`${styles.resourceLink} ${styles.code}`} href={`/app/sandboxes/${encodeURIComponent(workerId)}`}>{workerId}</Link></li>)}</ul>
      <p className={styles.note}>Open a worker’s Jobs section with a job ID to read logs and results. Worker files remain local. RAM is not pooled.</p>
      <dl className={styles.timestamps}>
        <div><dt>Created</dt><dd><Timestamp value={shown.createdAt} /></dd></div>
        {shown.startedAt ? <div><dt>Started</dt><dd><Timestamp value={shown.startedAt} /></dd></div> : null}
        {shown.finishedAt ? <div><dt>Finished</dt><dd><Timestamp value={shown.finishedAt} /></dd></div> : null}
      </dl>
    </> : null}
    {confirming && canCancel ? <CancelSuperSandboxDialog workspaceId={workspaceId} id={id} onClose={() => setConfirming(false)} onCancelled={next => {
      setAck({ baseline: run, response: next });
      setConfirming(false);
      resource.refresh();
      // The trigger may disappear once the batch turns terminal; land on the
      // status heading instead of losing focus to <body>.
      requestAnimationFrame(() => heading.current?.focus({ preventScroll: true }));
    }} /> : null}
  </section>;
}

function CopyJobId({ jobId }: { jobId: string }) {
  const [copied, setCopied] = useState(false);
  return <button type="button" className={styles.resourceLink} onClick={async () => {
    try {
      await navigator.clipboard.writeText(jobId);
    } catch {
      const area = document.createElement("textarea");
      area.value = jobId;
      document.body.appendChild(area);
      area.select();
      document.execCommand("copy");
      area.remove();
    }
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  }}>{copied ? "Copied" : "Copy"}</button>;
}

function Timestamp({ value }: { value: string }) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? <>{value}</> : <time dateTime={value}>{date.toLocaleString()}</time>;
}
