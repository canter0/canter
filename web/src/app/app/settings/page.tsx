"use client";
import Link from "next/link";
import { SettingsShell, SettingRow } from "@/components/settings-shell";
import { useWorkspace } from "@/components/workspace-context";
import styles from "@/components/settings.module.css";
export default function GeneralSettingsPage() {
  const { data, error } = useWorkspace();
  return <SettingsShell active="General" title="General" description="Your workspace, shared resources, and access.">
    {error ? <p className={styles.notice} role="alert">{String(error)}</p> : null}
    <section className={styles.section}><h2>Workspace</h2><div className={styles.card}>
      <SettingRow title="Workspace name" description="The workspace you’re working in">{data?.workspace.name ?? "Loading…"}</SettingRow>
      <SettingRow title="Your role" description="Your access to this workspace">{data?.workspace.role ?? "—"}</SettingRow>
      <SettingRow title="Workspace ID" description="Use this to connect an agent"><span className={styles.muted}>{data?.workspace.id ?? "—"}</span></SettingRow>
    </div></section>
    <section className={styles.section}><h2>Resources & access</h2><div className={styles.card}>
      <SettingRow title="Shared secrets" description="Keep integration credentials in one place."><Link className={styles.button} href="/app/settings/secrets">Manage secrets</Link></SettingRow>
      <SettingRow title="Agent access" description="Manage each agent’s permissions and revoke access."><Link className={styles.button} href="/app/settings/agent">Manage agents</Link></SettingRow>
      <SettingRow title="Plans & usage" description="Review workspace usage and payment details."><Link className={styles.button} href="/app/billing">View usage</Link></SettingRow>
    </div></section>
  </SettingsShell>;
}
