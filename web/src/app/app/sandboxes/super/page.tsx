"use client";

import { AppShell } from "@/components/app-shell";
import { SuperSandboxDashboard } from "@/components/super-sandbox-dashboard";
import { useWorkspace } from "@/components/workspace-context";
import styles from "@/components/workspace.module.css";

export default function SuperSandboxesPage() {
  const { data, error } = useWorkspace();
  return <AppShell active="Sandboxes" pageTitle="Super Sandboxes" pageKey={data?.workspace.id}>
    <section className={styles.contentPage}>{data
      ? <SuperSandboxDashboard key={`${data.account.id}:${data.workspace.id}`} workspaceId={data.workspace.id} />
      : <p role={error ? "alert" : "status"}>{error ? "Couldn’t load this organisation." : "Loading Super Sandboxes…"}</p>}
    </section>
  </AppShell>;
}
