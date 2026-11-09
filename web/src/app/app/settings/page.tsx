"use client";
import Link from "next/link";
import { useState, type FormEvent } from "react";
import { SettingsShell, SettingRow } from "@/components/settings-shell";
import { CopyValue } from "@/components/copy-value";
import { useWorkspace } from "@/components/workspace-context";
import { canterFetch, type Workspace } from "@/lib/canter-api";
import { workspaceDisplayName } from "@/lib/workspace-selection";
import styles from "@/components/settings.module.css";

function OrganizationName({ workspace, onRenamed }: { workspace: Workspace; onRenamed: () => void }) {
  const [name, setName] = useState(workspace.name);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  async function rename(event: FormEvent) {
    event.preventDefault();
    if (pending) return;
    setPending(true); setError(""); setSaved(false);
    try { await canterFetch(`/workspaces/${encodeURIComponent(workspace.id)}`, { method: "PATCH", body: JSON.stringify({ name }) }); setSaved(true); onRenamed(); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Couldn’t rename this organization."); }
    finally { setPending(false); }
  }
  if (workspace.role !== "owner") return <>{workspaceDisplayName(workspace.name)}</>;
  return <form onSubmit={rename} className={styles.inlineForm}>
    <input type="text" required maxLength={64} aria-label="Organization name" value={name} onChange={event => { setName(event.target.value); setSaved(false); }} />
    <button disabled={pending || !name.trim() || name === workspace.name}>{pending ? "Saving…" : "Rename"}</button>
    {saved ? <span role="status">Saved.</span> : null}
    {error ? <span role="alert">{error}</span> : null}
  </form>;
}

export default function GeneralSettingsPage() {
  const { data, error, retry } = useWorkspace();
  return <SettingsShell active="General" title="General" description="Your organization, shared resources, and access.">
    {error ? <p className={styles.notice} role="alert">{String(error)}</p> : null}
    <section className={styles.section}><h2>Organization</h2><div className={styles.card}>
      <SettingRow title="Organization name" description="The organization you’re working in">{data ? <OrganizationName workspace={data.workspace} onRenamed={retry} /> : "Loading…"}</SettingRow>
      <SettingRow title="Your role" description="Your access to this organization">{data?.workspace.role ?? "—"}</SettingRow>
      <SettingRow title="Organization ID" description="Use this to connect an agent"><CopyValue value={data?.workspace.id} label="Organization ID" /></SettingRow>
    </div></section>
    <section className={styles.section}><h2>Resources & access</h2><div className={styles.card}>
      <SettingRow title="Shared secrets" description="Keep integration credentials in one place."><Link className={styles.button} href="/app/settings/secrets">Manage secrets</Link></SettingRow>
      <SettingRow title="People" description="Invite people and manage their roles."><Link className={styles.button} href="/app/settings/people">Manage people</Link></SettingRow>
      <SettingRow title="Agent access" description="Manage each agent’s permissions and revoke access."><Link className={styles.button} href="/app/settings/agent">Manage agents</Link></SettingRow>
      <SettingRow title="Plans & usage" description="Review organization usage and payment details."><Link className={styles.button} href="/app/billing">View usage</Link></SettingRow>
    </div></section>
  </SettingsShell>;
}
