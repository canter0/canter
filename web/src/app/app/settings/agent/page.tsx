"use client";

import { useState } from "react";
import { AgentPermissions } from "@/components/agent-permissions";
import { ConnectAgentButton } from "@/components/connect-agent-button";
import { SettingsShell } from "@/components/settings-shell";
import { useWorkspace } from "@/components/workspace-context";
import { agentIsConnected, canterFetch, type Installation } from "@/lib/canter-api";
import styles from "@/components/settings.module.css";

function AgentAccess({ agent, editable, refresh, onSaved }: { agent: Installation; editable: boolean; refresh: () => void; onSaved: (message: string) => void }) {
  const [authority, setAuthority] = useState(agent.authority);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const active = agentIsConnected(agent);
  const changed = JSON.stringify(authority) !== JSON.stringify(agent.authority);
  async function update(revoke = false) {
    setPending(true); setError(""); onSaved("");
    try {
      await canterFetch(`/installations/${encodeURIComponent(agent.id)}?workspaceId=${encodeURIComponent(agent.workspaceId)}`, {
        method: revoke ? "DELETE" : "PATCH",
        ...(!revoke ? { body: JSON.stringify({ authority }) } : {}),
      });
      onSaved(revoke ? `${agent.name}’s access revoked.` : `${agent.name}’s permissions saved.`); refresh();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Couldn’t update this agent."); }
    finally { setPending(false); }
  }
  return <section className={styles.card} id={agent.id} style={{ marginBottom: 16 }} aria-label={`${agent.name} permissions`}>
    <div className={styles.row}><div><h3>{agent.name}</h3><p>{agent.harness} · {active ? agent.expiresAt ? "Temporary access" : "Access until revoked" : agent.revokedAt ? "Revoked" : "Expired"}</p></div><span className={styles.badge}>{active ? "Authorized" : "Access ended"}</span></div>
    {active ? <><AgentPermissions value={authority} onChange={value => { setAuthority(value); onSaved(""); }} disabled={!editable || pending} />
      <div className={`${styles.row} ${styles.agentFooter}`}><p className={styles.muted}>{agent.workers?.length ? `Also limits ${agent.workers.length} worker${agent.workers.length === 1 ? "" : "s"} under this connection.` : "Changes take effect on the agent’s next request."}</p><div className={styles.actions}><button className={styles.button} disabled={!editable || pending} onClick={() => void update(true)}>Revoke access</button><button className={styles.primary} disabled={!editable || pending || !changed} onClick={() => void update()}>{pending ? "Saving…" : "Save permissions"}</button></div></div>
    </> : <p className={styles.muted} style={{ padding: "16px 0" }}>Connect this agent again to give it access.</p>}
    {error ? <p role="alert" className={`${styles.notice} ${styles.error}`}>{error}</p> : null}
  </section>;
}

export default function AgentSettingsPage() {
  const { data, error, loading, retry } = useWorkspace();
  const [notice, setNotice] = useState("");
  const agents = data?.installations.filter(agent => agent.harness !== "canter-hosted") ?? [];
  const editable = data?.workspace.role === "owner";
  return <SettingsShell active="Agents" title="Agents" description="Choose what each agent can read and change in your workspace.">
    <div className={styles.toolbar}><p className={styles.muted}>{agents.length} agent{agents.length === 1 ? "" : "s"}</p>{editable ? <ConnectAgentButton className={styles.primary}>Connect agent</ConnectAgentButton> : null}</div>
    {notice ? <p className={styles.notice} role="status">{notice}</p> : null}
    {loading ? <p role="status">Loading agents…</p> : error ? <p role="alert" className={`${styles.notice} ${styles.error}`}>Couldn’t load agents.<button onClick={retry}>Try again</button></p> : <>
      {!editable ? <p className={styles.notice}>Only workspace owners can change agent permissions.</p> : null}
      {agents.length ? agents.map(agent => <AgentAccess key={`${agent.id}:${JSON.stringify(agent.authority)}:${agent.revokedAt}`} agent={agent} editable={editable} refresh={retry} onSaved={setNotice} />) : <div className={styles.empty}><h2>No agents connected</h2><p>Connect an agent and choose its permissions here.</p></div>}
    </>}
  </SettingsShell>;
}
