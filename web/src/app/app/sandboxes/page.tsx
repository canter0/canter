"use client";

import { AppShell } from "@/components/app-shell";
import { SandboxDashboard } from "@/components/sandbox-dashboard";
import { useWorkspace } from "@/components/workspace-context";
import styles from "@/components/workspace.module.css";

export default function SandboxesPage() {
  const { data, error, loading } = useWorkspace();
  if (!data) return <AppShell active="Sandboxes" pageTitle="Sandboxes"><section className={styles.contentPage}>
    <p role={error ? "alert" : "status"}>{loading ? "Loading sandboxes…" : "Couldn’t load this organization."}</p>
  </section></AppShell>;
  return <AppShell active="Sandboxes" pageTitle="Sandboxes" pageKey={data.workspace.id}><section className={styles.contentPage}>
    <SandboxDashboard key={data.workspace.id} workspaceId={data.workspace.id} />
  </section></AppShell>;
}
