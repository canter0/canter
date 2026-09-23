"use client";

import Link from "next/link";
import { useState } from "react";
import { AgentPermissions } from "@/components/agent-permissions";
import { SettingsShell } from "@/components/settings-shell";
import { useWorkspace } from "@/components/workspace-context";
import { agentIsConnected, canterFetch, type Authority, type Installation } from "@/lib/canter-api";
import styles from "@/components/settings.module.css";

function WorkspaceDefaults({ workspaceId, value, editable, refresh, onSaved }: { workspaceId: string; value: Authority; editable: boolean; refresh: () => void; onSaved: (message: string) => void }) {
  const [authority, setAuthority] = useState(value);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  async function save() {
    setPending(true); setError(""); onSaved("");
    try {
      await canterFetch(`/workspaces/${encodeURIComponent(workspaceId)}/agent-settings`, { method: "PATCH", body: JSON.stringify({ authority }) });
      onSaved("Workspace defaults saved."); refresh();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Couldn’t save workspace defaults."); }
    finally { setPending(false); }
  }
  return <section className={styles.section} aria-label="Workspace agent defaults">
    <h2>Workspace defaults</h2>
    <div className={styles.card}>
      <AgentPermissions value={authority} onChange={setAuthority} disabled={!editable || pending} />
      <div className={`${styles.row} ${styles.agentFooter}`}><p className={styles.muted}>Applies to new agents and agents using workspace defaults. Individual overrides stay as they are.</p><button className={styles.primary} disabled={!editable || pending || JSON.stringify(value) === JSON.stringify(authority)} onClick={() => void save()}>{pending ? "Saving…" : "Save defaults"}</button></div>
      {error ? <p className={`${styles.notice} ${styles.error}`} role="alert">{error}</p> : null}
    </div>
  </section>;
}

function AgentAccess({ agent, defaults, editable, refresh, onSaved }: { agent: Installation; defaults: Authority; editable: boolean; refresh: () => void; onSaved: (message: string) => void }) {
  const [authority, setAuthority] = useState(agent.authority);
  const [inherit, setInherit] = useState(!!agent.useWorkspaceAuthority);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const active = agentIsConnected(agent);
  const changed = inherit !== !!agent.useWorkspaceAuthority || (!inherit && JSON.stringify(authority) !== JSON.stringify(agent.authority));
  async function update() {
    setPending(true); setError(""); onSaved("");
    try {
      await canterFetch(`/installations/${encodeURIComponent(agent.id)}?workspaceId=${encodeURIComponent(agent.workspaceId)}`, { method: "PATCH", body: JSON.stringify({ authority, useWorkspaceAuthority: inherit }) });
      onSaved(`${agent.name}’s permissions saved.`); refresh();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Couldn’t update this agent."); }
    finally { setPending(false); }
  }
  return <section className={styles.card} id={agent.id} style={{ marginBottom: 16 }} aria-label={`${agent.name} permissions`}>
    <div className={styles.row}><div><h3>{agent.name}</h3><p>{agent.harness} · {active ? agent.expiresAt ? "Temporary access" : "Access until revoked" : agent.revokedAt ? "Revoked" : "Expired"}</p></div><span className={styles.badge}>{active ? inherit ? "Workspace defaults" : "Individual override" : "Access ended"}</span></div>
    {active ? <>
      <label className={styles.inheritPermissions}><input type="checkbox" checked={inherit} disabled={!editable || pending} onChange={event => { setInherit(event.target.checked); onSaved(""); }} />Use workspace defaults</label>
      <AgentPermissions value={inherit ? defaults : authority} onChange={value => { setAuthority(value); onSaved(""); }} disabled={!editable || pending || inherit} />
      <div className={`${styles.row} ${styles.agentFooter}`}><p className={styles.muted}>Changes take effect on the agent’s next request.</p><button className={styles.primary} disabled={!editable || pending || !changed} onClick={() => void update()}>{pending ? "Saving…" : "Save permissions"}</button></div>
    </> : <p className={styles.muted} style={{ padding: "16px 0" }}>Reconnect this agent from <Link href="/app/agents">Your agents</Link>.</p>}
    {error ? <p role="alert" className={`${styles.notice} ${styles.error}`}>{error}</p> : null}
  </section>;
}

export default function AgentSettingsPage() {
  const { data, error, loading, retry } = useWorkspace();
  const [notice, setNotice] = useState("");
  const agents = data?.installations.filter(agent => agent.harness !== "canter-hosted") ?? [];
  const editable = data?.workspace.role === "owner";
  return <SettingsShell active="Agents" title="Agents" description="Set workspace defaults or customize an individual agent’s permissions.">
    <div className={styles.toolbar}><p className={styles.muted}>Agent permissions</p><Link className={styles.button} href="/app/agents">View your agents</Link></div>
    {notice ? <p className={styles.notice} role="status">{notice}</p> : null}
    {loading ? <p role="status">Loading agents…</p> : error ? <p role="alert" className={`${styles.notice} ${styles.error}`}>Couldn’t load agents.<button onClick={retry}>Try again</button></p> : data ? <>
      {!editable ? <p className={styles.notice}>Only workspace owners can change agent permissions.</p> : null}
      <WorkspaceDefaults key={`${data.workspace.id}:${JSON.stringify(data.workspace.agentAuthority)}`} workspaceId={data.workspace.id} value={data.workspace.agentAuthority} editable={editable} refresh={retry} onSaved={setNotice} />
      <section className={styles.section}><h2>Individual agents</h2>
      {agents.length ? agents.map(agent => <AgentAccess key={`${agent.id}:${JSON.stringify(agent.authority)}:${agent.useWorkspaceAuthority}:${agent.revokedAt}`} agent={agent} defaults={data.workspace.agentAuthority} editable={editable} refresh={retry} onSaved={setNotice} />) : <p className={styles.muted}>No agents connected. <Link href="/app/agents">Connect one from Your agents</Link>.</p>}
      </section>
    </> : null}
  </SettingsShell>;
}
