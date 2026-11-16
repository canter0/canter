"use client";

import { useParams } from "next/navigation";
import { AppShell } from "@/components/app-shell";
import { SandboxWorkbench } from "@/components/sandbox-workbench";
import { useWorkspace } from "@/components/workspace-context";
import styles from "@/components/workspace.module.css";

export default function SandboxDetailPage() {
  const { id } = useParams<{ id: string }>();
  const { data, error } = useWorkspace();
  if (!data) return <AppShell active="Sandboxes"><div className={styles.contentPage}><p role={error ? "alert" : "status"}>{error ? "Couldn’t load this organization." : "Loading…"}</p></div></AppShell>;
  // Keying on workspace:id fully resets all sandbox state — in-flight reads can
  // never repaint into another organization or machine.
  return <AppShell active="Sandboxes" pageTitle="Sandbox" pageKey={`${data.workspace.id}:${id}`}><section className={styles.contentPage}>
    <SandboxWorkbench key={`${data.workspace.id}:${id}`} workspaceId={data.workspace.id} id={id} />
  </section></AppShell>;
}
