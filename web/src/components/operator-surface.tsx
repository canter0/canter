"use client";
import Link from "next/link";
import { useEffect, useState } from "react";
import { canterFetch, type SystemRecord, type Installation, type InitialDeploymentSummary, type ChangeSummary, type WorkspaceAction } from "@/lib/canter-api";
import { type OperatorSurface, surfaceKey } from "@/lib/operator-api";
import { InitialDeploymentReview } from "./initial-deployment-review";
import { ChangeReview } from "./change-review";
import { SystemDetail } from "./system-detail";
import { BillingSettings } from "./billing-settings";
import { EmbeddedAppSurface } from "./embedded-app-surface";
import { ActionFeed } from "./action-feed";
import { GitHubRepositories } from "./github-repositories";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./operator-workspace.module.css";

type Lists = { systems?: SystemRecord[]; installations?: Installation[]; initialDeployments?: InitialDeploymentSummary[]; changes?: ChangeSummary[]; actions?: WorkspaceAction[] };
export function OperatorSurfaceView({ surface, workspaceId, onSelect, conversationId, githubResult, busy, onDeploy }: { surface: OperatorSurface; workspaceId: string; onSelect: (surface: OperatorSurface) => void; conversationId?: string; githubResult?: string; busy: boolean; onDeploy: (repository: string) => Promise<void> }) {
  const [data, setData] = useState<Lists | null>(null);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    if (!["apps", "deployments", "activity", "agents"].includes(surface.kind)) return;
    const controller = new AbortController();
    const base = `/workspaces/${encodeURIComponent(workspaceId)}`;
    let busy = false;
    async function refresh() {
      if (busy || controller.signal.aborted) return; busy = true;
      try {
        let next: Lists;
        if (surface.kind === "deployments") {
          const [initial, changes] = await Promise.all([canterFetch<Lists>(`${base}/initial-deployments`, { signal: controller.signal }), canterFetch<Lists>(`${base}/changes`, { signal: controller.signal })]);
          next = { ...initial, ...changes };
        } else next = await canterFetch<Lists>(surface.kind === "agents" ? `/installations?workspaceId=${encodeURIComponent(workspaceId)}` : `${base}/${surface.kind === "apps" ? "systems" : "activity"}`, { signal: controller.signal });
        if (!controller.signal.aborted) { setData(next); setError(""); }
      } catch (cause) { if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "This view could not be loaded."); }
      finally { busy = false; }
    }
    void refresh(); const timer = setInterval(() => { if (document.visibilityState === "visible") void refresh(); }, 5000);
    return () => { controller.abort(); clearInterval(timer); };
  }, [surface.kind, workspaceId, retry]);
  return <EmbeddedAppSurface workspaceId={workspaceId}><div className={styles.embedded} key={surfaceKey(surface)}>
    {surface.kind === "github" ? <GitHubRepositories workspaceId={workspaceId} conversationId={conversationId} result={githubResult} busy={busy} onDeploy={onDeploy} /> : null}
    {surface.kind === "billing" ? <BillingSettings initialPlan="payg" checkoutReturned={false} /> : null}
    {surface.kind === "deployment" && surface.id ? <InitialDeploymentReview id={surface.id} /> : null}
    {surface.kind === "change" && surface.id && surface.system ? <ChangeReview id={surface.id} system={surface.system} /> : null}
    {surface.kind === "app" && surface.system ? <SystemDetail name={surface.system} /> : null}
    {surface.kind === "repository" ? <div className={styles.resourceBody}><h2>{surface.repository}</h2><p>GitHub repository</p><dl><dt>Inspected commit</dt><dd className={styles.digest}>{surface.id}</dd></dl><a href={`https://github.com/${surface.repository}/tree/${surface.id}`} target="_blank" rel="noreferrer">Open source <WorkspaceIcon name="external" width="14" height="14" /></a><p className={styles.note}>Ask Canter to read a file, explain the runtime, or prepare a supported deployment.</p></div> : null}
    {error ? <p className={styles.error} role="alert">{error} <button onClick={() => setRetry(value => value + 1)}>Retry</button></p> : null}
    {!data && !error && ["apps", "deployments", "activity", "agents"].includes(surface.kind) ? <p className={styles.note} role="status">Loading current workspace state…</p> : null}
    {data ? <div className={styles.resourceBody}>
      {surface.kind === "apps" ? <><h2>Apps</h2>{data.systems?.length ? data.systems.map(item => <button key={item.contract.metadata.name} className={styles.resourceRow} onClick={() => onSelect({ kind: "app", system: item.contract.metadata.name })}><span>{item.contract.metadata.name}<small>{item.contract.spec.intent}</small></span><WorkspaceIcon name="chevron" /></button>) : <p className={styles.note}>No apps have been deployed in this workspace.</p>}</> : null}
      {surface.kind === "deployments" ? <><h2>Deployments</h2>{data.initialDeployments?.map(item => <button key={item.id} className={styles.resourceRow} onClick={() => onSelect({ kind: "deployment", id: item.id })}><span>{item.system}<small>{item.summary}</small></span><small>{item.phase}</small></button>)}{data.changes?.map(item => <button key={item.id} className={styles.resourceRow} onClick={() => onSelect({ kind: "change", id: item.id, system: item.system })}><span>{item.system}<small>{item.summary}</small></span><small>{item.phase}</small></button>)}{!data.initialDeployments?.length && !data.changes?.length ? <p className={styles.note}>No deployment proposals or changes yet.</p> : null}</> : null}
      {surface.kind === "activity" ? <><h2>Activity</h2><ActionFeed actions={data.actions ?? []} />{!data.actions?.length ? <p className={styles.note}>No recorded actions yet.</p> : null}</> : null}
      {surface.kind === "agents" ? <><h2>Agent access</h2>{data.installations?.map(item => <Link key={item.id} href={`/app/agents/${encodeURIComponent(item.id)}`} className={styles.resourceRow}><span>{item.name}<small>{item.harness} · {item.authority.draft ? "Inspect and prepare" : "Inspect only"}</small></span><small>{item.revokedAt ? "Revoked" : "Authorized"}</small></Link>)}<p className={styles.note}>Open an agent to inspect or revoke its access.</p></> : null}
    </div> : null}
  </div></EmbeddedAppSurface>;
}
