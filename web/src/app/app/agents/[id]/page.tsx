"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useState } from "react";
import { AppShell } from "@/components/app-shell";
import { useWorkspace } from "@/components/workspace-context";
import { WorkspaceIcon, type WorkspaceIconName } from "@/components/workspace-icon";
import { agentIsConnected, canterFetch, relativeTime } from "@/lib/canter-api";
import styles from "./agent-detail.module.css";

function dateLabel(value: string) {
  return new Date(value).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" });
}

function seenLabel(value?: string | null) {
  if (!value) return "Not seen yet";
  const time = relativeTime(value);
  return time === "now" ? "Just now" : `${time} ago`;
}

function Permission({ icon, title, description, value, tone }: { icon: WorkspaceIconName; title: string; description: string; value: string; tone?: "allowed" | "approval" }) {
  return <li className={styles.permission}>
    <WorkspaceIcon name={icon} width="19" height="19" />
    <div className={styles.permissionText}><h3>{title}</h3><p>{description}</p></div>
    <span className={styles.permissionValue} data-tone={tone}>{tone === "allowed" ? <WorkspaceIcon name="check" width="14" height="14" /> : null}{value}</span>
  </li>;
}

export default function AgentInstallationPage() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const { data, error, loading, retry } = useWorkspace();
  const [revokeError, setRevokeError] = useState("");
  const [pending, setPending] = useState(false);
  const installation = data?.installations.find(item => item.id === id);
  const authorized = installation ? agentIsConnected(installation) : false;
  const status = installation?.revokedAt ? "Access revoked" : authorized ? "Authorized" : "Expired";
  const requiresApproval = installation?.authority.applyMode === "human-approval-required";
  const applyMode = installation?.authority.applyMode ?? "never";
  const applyLabel = !authorized || applyMode === "never" ? "Not allowed" : requiresApproval ? "Approval required" : applyMode === "automatic" ? "Without asking" : applyMode.replaceAll("-", " ");

  async function revoke() {
    if (!installation || pending || !authorized) return;
    setPending(true);
    setRevokeError("");
    try {
      await canterFetch(`/installations/${encodeURIComponent(installation.id)}?workspaceId=${encodeURIComponent(installation.workspaceId)}`, { method: "DELETE" });
      retry();
      router.push("/app/agents");
      router.refresh();
    } catch (cause) {
      setRevokeError(cause instanceof Error ? cause.message : "Canter could not revoke this agent’s access.");
      setPending(false);
    }
  }

  return <AppShell active="Agents">
    <section className={styles.page}>
      <Link href="/app/agents" className={styles.back}><WorkspaceIcon name="right" width="15" height="15" />All agents</Link>
      {loading ? <div className={styles.empty} role="status">Loading agent details…</div> : !installation ? <div className={styles.empty}>
        <WorkspaceIcon name="agent" width="28" height="28" />
        <h1>{error ? "Couldn’t load this agent" : "Agent not found"}</h1>
        <p>{error || "This agent is no longer available in your workspace."}</p>
        <button className={styles.button} onClick={retry}>Try again</button>
      </div> : <>
        <header className={styles.header}>
          <span className={styles.avatar}><WorkspaceIcon name="agent" width="28" height="28" /></span>
          <div className={styles.identity}>
            <div className={styles.titleRow}><h1>{installation.name}</h1><span className={styles.status} data-active={authorized}><span />{status}</span></div>
            <p>Manage this agent’s access to your workspace.</p>
          </div>
        </header>

        {error ? <p className={styles.error} role="alert">Couldn’t refresh agent details. <button onClick={retry}>Try again</button></p> : null}

        <div className={styles.columns}>
          <div className={styles.primary}>
            <section aria-labelledby="permissions-heading">
              <div className={styles.sectionHeading}><h2 id="permissions-heading">Permissions</h2><p>{authorized ? "What this agent can do in your workspace." : "This agent no longer has access to your workspace."}</p></div>
              <Link className={styles.button} href={`/app/settings/agent#${encodeURIComponent(installation.id)}`}>Edit permissions</Link>
              <ul className={styles.permissions}>
                <Permission icon="apps" title="Inspect apps" description="View your apps, their status, and configuration." value={authorized && installation.authority.inspect ? "Allowed" : "Not allowed"} tone={authorized && installation.authority.inspect ? "allowed" : undefined} />
                <Permission icon="file" title="Prepare changes" description="Draft deployments and updates for review." value={authorized && installation.authority.draft ? "Allowed" : "Not allowed"} tone={authorized && installation.authority.draft ? "allowed" : undefined} />
                <Permission icon="activity" title="Apply changes" description={requiresApproval ? "You review and approve changes before they run." : "Changes follow this agent’s authorization rules."} value={applyLabel} tone={authorized && requiresApproval ? "approval" : undefined} />
                <Permission icon="settings" title="Provider credentials" description="Your cloud credentials stay private in Canter." value="Never shared" />
              </ul>
              {authorized && requiresApproval ? <div className={styles.approvalNote}><WorkspaceIcon name="check" width="16" height="16" /><p>Your agent prepares the change. You decide when it runs.</p></div> : null}
            </section>

            {installation.workers?.length ? <section className={styles.workers} aria-labelledby="workers-heading">
              <div className={styles.sectionHeading}><h2 id="workers-heading">Workers <span>{installation.workers.length}</span></h2><p>Additional agents working under this connection.</p></div>
              <ul>{installation.workers.map(worker => <li key={worker.id}><WorkspaceIcon name="agent" /><div><span>{worker.workerName}</span><small>Expires {new Date(worker.expiresAt).toLocaleString()}</small></div><span className={styles.permissionValue}>{authorized ? worker.workerDraft ? "Can prepare changes" : "Read only" : "Access ended"}</span></li>)}</ul>
            </section> : null}

            <section className={styles.continuity} aria-labelledby="access-heading">
              <div className={styles.sectionHeading}><h2 id="access-heading">{authorized ? installation.expiresAt ? "Temporary access" : "Remembered connection" : "Access ended"}</h2></div>
              <p>{!authorized ? "This agent needs a new connection to access your workspace again." : installation.expiresAt ? "This connection ends when its task finishes or its access expires, whichever comes first." : "This agent can reconnect without another setup until you revoke its access."} Your task history and activity stay in Canter.</p>
            </section>
          </div>

          <aside className={styles.connection} aria-labelledby="connection-heading">
            <h2 id="connection-heading">Connection details</h2>
            <dl>
              <div><dt>Agent client</dt><dd>{installation.harness || "Not specified"}</dd></div>
              <div><dt>Added on</dt><dd><time dateTime={installation.createdAt} title={new Date(installation.createdAt).toLocaleString()}>{dateLabel(installation.createdAt)}</time></dd></div>
              <div><dt>Last seen</dt><dd>{installation.lastSeenAt ? <time dateTime={installation.lastSeenAt} title={new Date(installation.lastSeenAt).toLocaleString()}>{seenLabel(installation.lastSeenAt)}</time> : "Not seen yet"}</dd></div>
              <div><dt>Active sessions</dt><dd>{installation.activeSessions ?? 0}</dd></div>
              <div><dt>Access</dt><dd>{!authorized ? installation.revokedAt ? "Revoked" : "Expired" : installation.expiresAt ? "Temporary" : "Until revoked"}</dd></div>
              {installation.expiresAt ? <div><dt>Expires</dt><dd><time dateTime={installation.expiresAt}>{new Date(installation.expiresAt).toLocaleString()}</time></dd></div> : null}
            </dl>
            <p className={styles.connectionHint}>{!authorized ? "This connection is no longer authorized." : installation.activeSessions ? "This agent has an active session with Canter." : "Authorized to connect. No active sessions right now."}</p>
            <div className={styles.agentId}><span>Agent ID</span><code>{installation.id}</code></div>
          </aside>
        </div>

        <footer className={styles.footer}>
          <div><h2>Revoke access</h2><p>{authorized ? "Disconnect this agent and its workers. Your apps and history stay." : "This agent’s access has already ended. Your apps and history stay."}</p></div>
          <button onClick={() => void revoke()} disabled={pending || !authorized} className={`${styles.button} ${styles.revoke}`}>{pending ? "Revoking…" : "Revoke access"}</button>
        </footer>
        {revokeError ? <p className={styles.error} role="alert">{revokeError}</p> : null}
      </>}
    </section>
  </AppShell>;
}
