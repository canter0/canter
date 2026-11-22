"use client";

import Link from "next/link";
import { useState } from "react";
import type { OperatorSurface } from "@/lib/operator-api";
import { runtimeAllocation, superSandboxBlockerLabel, superSandboxRequestPrompt, superSandboxStateLabel, superSandboxTerminal, type SuperSandboxState, type SuperSandboxSummaryResponse } from "@/lib/super-sandboxes";
import { useWrenResource, workBase } from "@/lib/wren-work";
import { workspaceDisplayName } from "@/lib/workspace-selection";
import { DashboardRequestAction } from "./dashboard-request-action";
import { useWorkspace } from "./workspace-context";
import { SandboxSearch, SandboxSelect } from "./sandbox-controls";
import styles from "./super-sandbox.module.css";
import shared from "./workspace.module.css";
import consoleStyles from "./sandbox-console.module.css";

// Poll only while the visible first page still contains unsettled work.
const hasUnsettledSummaries = (value: SuperSandboxSummaryResponse) => value.superSandboxes.some(run => !superSandboxTerminal(run.state));
type Props = { workspaceId: string; onSelect?: (surface: OperatorSurface) => void; onRequest?: (prompt: string) => void };

export function SuperSandboxDashboard(props: Props) {
  const { data } = useWorkspace();
  return <SuperSandboxDashboardInner key={`${data?.account.id}:${props.workspaceId}`} {...props} />;
}

export function SuperSandboxRequest({ workspaceId, id, onRequest }: { workspaceId: string; id?: string; onRequest?: (prompt: string) => void }) {
  const { data } = useWorkspace();
  if (!data || data.workspace.id !== workspaceId || !["owner", "operator"].includes(data.workspace.role ?? "")) return null;
  const prompt = superSandboxRequestPrompt(workspaceId, workspaceDisplayName(data.workspace.name), id);
  const label = id ? "Request cancellation" : "Request a Super Sandbox";
  return onRequest
    ? <button type="button" className={shared.secondaryButton} onClick={() => onRequest(prompt)}>{label}</button>
    : <DashboardRequestAction label={label} prompt={prompt} surface={id ? { kind: "super-sandbox", id } : { kind: "super-sandboxes" }} className={shared.secondaryButton} />;
}

function SuperSandboxDashboardInner({ workspaceId, onSelect, onRequest }: Props) {
  const [cursor, setCursor] = useState("");
  const [history, setHistory] = useState<string[]>([]);
  const base = `${workBase(workspaceId)}/super-sandboxes?view=summary&limit=25`;
  // Older pages load on demand and never poll; the first page polls only
  // while it still shows unsettled work.
  const list = useWrenResource<SuperSandboxSummaryResponse>(base + (cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""), cursor ? undefined : 4000, hasUnsettledSummaries);
  const result = list.data;
  const [search, setSearch] = useState("");
  const [state, setState] = useState<SuperSandboxState | "">("");
  const runs = result?.superSandboxes ?? [];
  const states = [...new Set([...runs.map(run => run.state), ...(state ? [state] : [])])].sort();
  const visible = runs.filter(run => (!state || run.state === state) && `${run.name} ${run.id}`.toLowerCase().includes(search.trim().toLowerCase()));
  return <section className={styles.root} aria-label="Super Sandboxes dashboard">
    <header className={shared.pageHeading}>
      <h1>Super Sandboxes</h1>
      <Link href="/app" className={shared.primaryButton}>Ask Canter</Link>
    </header>
    {list.error ? <p role="alert" className={shared.error}>{result ? "Couldn’t refresh. Showing the last loaded batches. " : ""}{list.error} <button type="button" onClick={list.refresh}>Retry</button></p> : null}
    {!result && !list.error ? <p role="status" className={shared.loading}>Loading Super Sandboxes…</p> : null}
    {result ? <>
      {!result.enabled ? <p className={styles.note}>New Super Sandbox batches are disabled on this server. Existing batches remain available to inspect and cancel.</p> : null}
      {result.superSandboxes.length ? <>
        <div className={consoleStyles.toolbar}>
          <SandboxSearch label="Search this page of Super Sandboxes" placeholder="Search this page" value={search} onChange={setSearch} />
          <SandboxSelect label="Filter this page by state" value={state} onChange={next => setState(next as SuperSandboxState | "")} options={[
            { value: "", label: "All states on this page" }, ...states.map(value => ({ value, label: superSandboxStateLabel(value) })),
          ]} />
          <button type="button" className={shared.secondaryButton} onClick={list.refresh}>Refresh</button>
        </div>
        <p className={styles.note}>Search and filter apply to this page only, not the whole organisation.</p>
        {visible.length ? <div className={styles.tableScroll} role="region" aria-label="Super Sandboxes" tabIndex={0}>
          <table className={`${styles.table} ${styles.batchTable}`} role="table">
            <thead><tr><th scope="col">Batch</th><th scope="col">State</th><th scope="col">Settled / total</th><th scope="col">Workers / concurrency</th><th scope="col">Runtime reserved / cap</th></tr></thead>
            <tbody>{visible.map(run => {
              const blocker = superSandboxBlockerLabel(run.blockedReason);
              return <tr key={run.id}>
                <th scope="row">{onSelect
                  ? <button type="button" className={styles.resourceLink} onClick={() => onSelect({ kind: "super-sandbox", id: run.id })}>{run.name}</button>
                  : <Link className={styles.resourceLink} href={`/app/sandboxes/super/${encodeURIComponent(run.id)}`}>{run.name}</Link>}
                  <small className={styles.identifier}>{run.id}</small>
                  {blocker ? <small className={`${shared.status} ${styles.annotation}`} data-phase="escalated">{blocker}</small> : null}</th>
                <td><span className={shared.status} data-phase={run.state === "needs_attention" ? "escalated" : run.state === "queued" ? "drafted" : run.state === "cancelling" ? "running" : run.state}>{superSandboxStateLabel(run.state)}</span></td>
                <td data-label="Settled / total">{run.progress.settled} / {run.progress.total} <small>· {run.progress.succeeded} succeeded</small>
                  {run.progress.unknown ? <small className={`${shared.status} ${styles.annotation}`} data-phase="escalated">{run.progress.unknown} unknown</small> : null}
                  {run.progress.outcomeUnavailable ? <small className={`${shared.status} ${styles.annotation}`} data-phase="failed">{run.progress.outcomeUnavailable} outcome unavailable</small> : null}</td>
                <td data-label="Workers / concurrency">{run.workerCount} / {run.maxConcurrency}</td>
                <td data-label="Runtime reserved / cap">{runtimeAllocation(run)}</td>
              </tr>;
            })}</tbody>
          </table>
        </div> : null}
        <div className={styles.actions}>
          {history.length ? <button type="button" className={shared.secondaryButton} onClick={() => {
            const previous = history[history.length - 1];
            setHistory(history.slice(0, -1));
            setCursor(previous);
          }}>← Newer batches</button> : null}
          {result.nextCursor ? <button type="button" className={shared.secondaryButton} onClick={() => {
            setHistory([...history, cursor]);
            setCursor(result.nextCursor);
          }}>Older batches →</button> : null}
        </div>
        <p className={styles.note}>Runtime is the worst-case sum of task timeouts, reserved upfront—not a spending cap. Worker VM billing continues separately.</p>
        {!visible.length ? <div className={consoleStyles.empty}>No matching Super Sandboxes <button type="button" className={shared.secondaryButton} onClick={() => { setSearch(""); setState(""); }}>Clear</button></div> : null}
      </> : <p className={consoleStyles.empty}>No Super Sandboxes yet</p>}
      {result.enabled ? <div className={styles.actions}><SuperSandboxRequest workspaceId={workspaceId} onRequest={onRequest} /></div> : null}
    </> : null}
  </section>;
}
