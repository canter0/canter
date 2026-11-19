"use client";
import {getSelectedWorkspace} from "@/lib/workspace-selection";

import { useEffect, useState } from "react";
import { useSurfaceWorkspace } from "./embedded-app-surface";
import Link from "next/link";
import { AppShell } from "@/components/app-shell";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./detail-page.module.css";
import { activityLabel, phaseTone } from "@/lib/workspace-overview";
import { canterFetch, relativeTime, type ChangeDetail } from "@/lib/canter-api";

export function ChangeReview({ id, system }: { id: string; system: string }) {
  const scopedWorkspace = useSurfaceWorkspace();
  const [workspaceId, setWorkspaceId] = useState("");
  const [change, setChange] = useState<ChangeDetail | null>(null);
  const [execution, setExecution] = useState<Record<string, unknown> | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");

  async function load(cancelled?: () => boolean) {
    const workspace = scopedWorkspace ? { id: scopedWorkspace } : (await getSelectedWorkspace()).workspace;
    if (!workspace) throw new Error("No organization is available.");
    const result = await canterFetch<ChangeDetail>(`/workspaces/${workspace.id}/systems/${encodeURIComponent(system)}/changes/${encodeURIComponent(id)}`);
    if (cancelled?.()) return;
    setWorkspaceId(workspace.id);
    setChange(result);
  }

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const workspace = scopedWorkspace ? { id: scopedWorkspace } : (await getSelectedWorkspace()).workspace;
        if (!workspace) throw new Error("No organization is available.");
        const result = await canterFetch<ChangeDetail>(`/workspaces/${workspace.id}/systems/${encodeURIComponent(system)}/changes/${encodeURIComponent(id)}`);
        if (cancelled) return;
        setWorkspaceId(workspace.id);
        setChange(result);
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "Canter could not load this Change.");
      }
    })();
    return () => { cancelled = true; };
  }, [id, system, scopedWorkspace]);

  async function authorize() {
    if (!change || !workspaceId || pending) return;
    setPending(true);
    setError("");
    try {
      const result = await canterFetch<ChangeDetail>(`/workspaces/${workspaceId}/systems/${encodeURIComponent(system)}/changes/${encodeURIComponent(id)}/authorize`, { method: "POST", body: JSON.stringify({ digest: change.digest }) });
      setChange(result);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not authorize this Change.");
    } finally {
      setPending(false);
    }
  }

  async function apply() {
    if (!change || !workspaceId || pending) return;
    setPending(true);
    setError("");
    try {
      const result = await canterFetch<Record<string, unknown>>(`/workspaces/${workspaceId}/systems/${encodeURIComponent(system)}/changes/${encodeURIComponent(id)}/apply`, { method: "POST" });
      setExecution(result);
      try { await load(); } catch { setError("Execution was accepted. Reload the review to see its latest state."); }
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not start this execution.");
    } finally {
      setPending(false);
    }
  }

  const impact = change?.plan?.impact;
  const assessment = change ? [
    ["digest", change.digest],
    ["base release", change.plan?.baseVersion ?? "none"],
	...(change.plan?.scale ? [["replica transition", `${change.plan.scale.service}: ${change.plan.scale.fromReplicas} → ${change.plan.scale.toReplicas}`]] : []),
	...(change.plan?.scale?.restoreAt ? [["automatic restore", `${change.plan.scale.restoreToReplicas} replicas at ${new Date(change.plan.scale.restoreAt).toLocaleString()}`]] : []),
    ["availability", impact?.availability ?? "not assessed"],
    ["data", impact?.data ?? "not assessed"],
    ["estimated monthly change (USD)", impact?.monthlyCostDeltaCents == null ? "not assessed" : `${impact.monthlyCostDeltaCents >= 0 ? "+" : "−"}$${Math.abs(impact.monthlyCostDeltaCents / 100).toFixed(2)}`],
  ] : [];

  const reload = () => { setError(""); void load().catch(cause => setError(cause instanceof Error ? cause.message : "Could not reload this change.")); };

  return (
    <AppShell active="Changes" context={`canter / default / ${id}`} pageTitle={change?.summary ?? "Change review"}>
      <section className={styles.page}><div className={styles.inner}>
        <header className={styles.header}>
          <div><div className={styles.eyebrow}>{system ? `${system} · ` : ""}Drafted by {change?.draftedBy?.displayName ?? change?.draftedBy?.id ?? "agent"}</div><h1>{change?.summary ?? "Loading change…"}</h1></div>
          {change ? <span className={styles.status} data-tone={phaseTone(change.phase)}><span />{activityLabel(change.phase)}</span> : null}
        </header>
        {error ? <div role="alert" className={styles.error}>{error} <button onClick={reload}>Reload</button></div> : null}
        <div className={styles.grid}>
          <section className={styles.section}><h2>What will happen<small>{change?.operations?.length ?? 0} steps</small></h2>
            {change?.operations?.length ? <ol className={styles.steps}>{change.operations.map((operation, index) => <li key={operation.id}><span className={styles.stepIndex}>{index + 1}</span><div>{operation.description}<small>{operation.kind}</small></div><span className={styles.stepPhase} data-tone={phaseTone(operation.phase)}>{activityLabel(operation.phase)}</span></li>)}</ol> : <p className={styles.empty}>{change ? "No steps recorded." : "Loading…"}</p>}
          </section>
          <section className={styles.section}><h2>Assessment</h2>
            <dl className={styles.rows}>{assessment.map(([key, value]) => <div key={key}><dt>{key.charAt(0).toUpperCase() + key.slice(1)}</dt><dd className={key === "digest" ? styles.mono : undefined}>{value}</dd></div>)}</dl>
          </section>
        </div>
        <div className={styles.stack}>
          <section className={styles.section}><h2>Execution evidence<small>{change?.evidence?.length ?? 0}</small></h2>
            {change?.evidence?.length ? <ul className={styles.list}>{change.evidence.map((record) => <li key={`${record.operationId}-${record.observedAt}`}><div>{record.statement}<small>{record.operationId}</small></div><small>{relativeTime(record.observedAt)}</small></li>)}</ul> : <p className={styles.empty}>No execution evidence yet.</p>}
          </section>
        </div>
        {execution ? <p role="status" className={styles.notice}>Execution accepted: {String(execution.id ?? execution.executionId ?? "durable record created")}</p> : null}
        <div className={styles.actions}>
          {change ? <Link href={`/app/system/${encodeURIComponent(system)}/policies?from=${encodeURIComponent(change.id)}`} className={styles.link}>Create a standing policy from this change<WorkspaceIcon name="right" width="14" height="14" /></Link> : <span />}
          <div className={styles.buttons}>
            {change?.phase === "drafted" ? <button onClick={() => void authorize()} disabled={pending} className={styles.primary}>{pending ? "Approving…" : "Approve this change"}</button> : null}
            {change?.phase === "authorized" ? <button onClick={() => void apply()} disabled={pending} className={styles.primary}>{pending ? "Starting…" : "Apply approved change"}<WorkspaceIcon name="right" width="16" height="16" /></button> : null}
          </div>
        </div>
      </div></section>
    </AppShell>
  );
}
