"use client";

import { WorkspaceIcon } from "./workspace-icon";
import type { InitialDeploymentDetail, InitialDeploymentExecution } from "@/lib/canter-api";
import styles from "./deployment-panel.module.css";

type Props = {
  deployment: InitialDeploymentDetail | null;
  execution: InitialDeploymentExecution | null;
  error: string;
  pending: boolean;
  canRetry: boolean;
  explanation: string;
  onApprove: () => void;
  onApply: () => void;
};

export function DeploymentPanel({ deployment, execution, error, pending, canRetry, explanation, onApprove, onApply }: Props) {
  if (!deployment) return <div className={styles.panel}><p className={styles.description} role={error ? "alert" : "status"}>{error || "Loading deployment…"}</p></div>;
  const plan = deployment.plan;
  const host = plan.system.spec.constraints?.host;
  const source = /^Serve ([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+) at commit ([a-f0-9]{40})$/.exec(plan.system.spec.intent ?? "");
  const phase = deployment.phase;
  const status = ({ drafted: "Ready for review", authorized: "Approved", queued: "Starting", running: "Deploying", succeeded: "Deployed", failed: "Needs attention" } as Record<string, string>)[phase] ?? phase;
  const descriptions: Record<string, string> = {
    drafted: "Your app is prepared. Review the details and deploy when you’re ready.",
    authorized: "This deployment is approved and ready to start.",
    queued: "Canter is starting your approved deployment.",
    running: "Canter is deploying your app and checking that it responds.",
    succeeded: "The deployment completed and its endpoint passed verification.",
    failed: "The deployment needs attention. The details below show what happened.",
  };
  const memory = host?.memoryMiB ? host.memoryMiB >= 1024 ? `${host.memoryMiB / 1024} GB memory` : `${host.memoryMiB} MB memory` : "";
  const action = phase === "drafted" ? onApprove : phase === "authorized" || canRetry ? onApply : undefined;
  const actionLabel = phase === "drafted" ? "Approve and deploy" : canRetry ? "Retry deployment" : "Start deployment";

  return <div className={styles.panel}>
    <div className={styles.identity}><span className={styles.appIcon}><WorkspaceIcon name="apps" width="25" height="25" /></span><span className={styles.status} data-phase={phase}><span />{status}</span></div>
    <h1>{deployment.system}</h1>
    <p className={styles.description}>{descriptions[phase] ?? deployment.summary}</p>
    {source ? <a className={styles.source} href={`https://github.com/${source[1]}/tree/${source[2]}`} target="_blank" rel="noreferrer"><WorkspaceIcon name="folder" /><span>{source[1]}<small>Commit {source[2].slice(0, 7)}</small></span><WorkspaceIcon name="external" width="16" height="16" /></a> : <p className={styles.description}>{deployment.summary}</p>}
    <div className={styles.facts}>
      <div><span>Compute</span><strong>{host?.count ?? 1} host{(host?.count ?? 1) === 1 ? "" : "s"}</strong><small>{[host?.class, memory].filter(Boolean).join(" · ")}</small></div>
      <div><span>Application</span><strong>{(plan.system.spec.services ?? []).map(service => service.name).join(", ") || "Not specified"}</strong><small>Port {plan.release.publicPort}</small></div>
    </div>
    {error || deployment.failure ? <p className={styles.error} role="alert">{error || deployment.failure}</p> : null}
    {action ? <div className={styles.approval}><button className={styles.primary} disabled={pending} onClick={action}>{pending ? "Starting deployment…" : actionLabel}<WorkspaceIcon name="right" width="17" height="17" /></button><p>{phase === "drafted" ? "Approves this exact version and starts the deployment." : explanation}</p></div> : null}
    {execution ? <p className={styles.description} role="status">Attempt {execution.attempts} · {execution.phase}</p> : null}
    <details className={styles.details}><summary>Deployment details<WorkspaceIcon name="down" width="15" height="15" /></summary><div>
      <h2>What will happen</h2>
      <ol className={styles.steps}>{deployment.operations.map((operation, index) => <li key={operation.id}><span className={styles.stepIndex}>{index + 1}</span><div>{operation.description}<small>{operation.failure || operation.phase}</small></div></li>)}</ol>
      <h2>Release</h2><dl className={styles.metadata}><div><dt>Command</dt><dd>{plan.release.command.join(" ")}</dd></div><div><dt>Health check</dt><dd>{plan.release.healthPath}</dd></div><div><dt>Verification</dt><dd>{plan.verification.method || "GET"} {plan.verification.path} → {plan.verification.expectedStatus}</dd></div><div><dt>Deployment ID</dt><dd>{deployment.id}</dd></div><div><dt>Approval digest</dt><dd>{deployment.digest}</dd></div><div><dt>Artifact digest</dt><dd>{plan.artifactSha256}</dd></div><div><dt>Approved by</dt><dd>{deployment.authorization?.authorizedBy?.displayName ?? deployment.authorization?.authorizedBy?.id ?? "Awaiting approval"}</dd></div></dl>
      {deployment.evidence?.length ? <><h2>Execution evidence</h2><ul className={styles.evidence}>{deployment.evidence.map(record => <li key={`${record.operationId}-${record.observedAt}`}>{record.statement}<small>{new Date(record.observedAt).toLocaleString()}</small></li>)}</ul></> : null}
    </div></details>
  </div>;
}
