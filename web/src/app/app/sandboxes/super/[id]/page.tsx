"use client";

import { useParams } from "next/navigation";
import { AppShell } from "@/components/app-shell";
import { SuperSandboxWorkbench } from "@/components/super-sandbox-workbench";
import { useWorkspace } from "@/components/workspace-context";
import styles from "@/components/workspace.module.css";

export default function SuperSandboxPage() {
  const { id } = useParams<{ id: string }>();
  const { data, error } = useWorkspace();
  return <AppShell active="Sandboxes" pageTitle="Super Sandbox" pageKey={`${data?.workspace.id}:${id}`}>
    <section className={styles.contentPage}>{data
      ? <SuperSandboxWorkbench key={`${data.account.id}:${data.workspace.id}:${id}`} workspaceId={data.workspace.id} id={id} />
      : <p role={error ? "alert" : "status"}>{error ? "Couldn’t load this organisation." : "Loading Super Sandbox…"}</p>}
    </section>
  </AppShell>;
}
