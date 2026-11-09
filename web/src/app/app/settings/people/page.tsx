"use client";
import Link from "next/link";
import { SettingsShell, SettingRow } from "@/components/settings-shell";
import { OrganizationPeople } from "@/components/organization-people";
import { useWorkspace } from "@/components/workspace-context";
import styles from "@/components/settings.module.css";
export default function PeopleSettingsPage() {
  const { data, error } = useWorkspace();
  return <SettingsShell active="People" title="People" description="The people who can work in this organization.">
    {error ? <p className={styles.notice} role="alert">{String(error)}</p> : null}
    {data ? <OrganizationPeople workspaceId={data.workspace.id} /> : null}
    <section className={styles.section}><div className={styles.card}>
      <SettingRow title="Agent access" description="Manage what connected agents can do."><Link className={styles.button} href="/app/settings/agent">Manage agents</Link></SettingRow>
    </div></section>
  </SettingsShell>;
}
