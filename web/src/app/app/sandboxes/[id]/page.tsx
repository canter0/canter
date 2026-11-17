"use client";

import { useParams, useSearchParams } from "next/navigation";
import { JobLinkWorkspace } from "@/components/job-link-workspace";
import { AppShell } from "@/components/app-shell";
import { SandboxWorkbench } from "@/components/sandbox-workbench";
import { useWorkspace } from "@/components/workspace-context";
import styles from "@/components/workspace.module.css";

export default function SandboxDetailPage() {
  const { id } = useParams<{ id: string }>();
  const { data, error } = useWorkspace();
  const params = useSearchParams();
  const linkWorkspace = params.get("historyJob") && params.get("workspace");
  if (!data) return <AppShell active="Sandboxes"><div className={styles.contentPage}><p role={error ? "alert" : "status"}>{error ? "Couldn’t load this organization." : "Loading…"}</p></div></AppShell>;
  if (linkWorkspace && linkWorkspace !== data.workspace.id) return <AppShell active="Sandboxes" pageTitle="Sandbox"><section className={styles.contentPage}>
    <JobLinkWorkspace id={linkWorkspace} memberships={data.workspaces} />
  </section></AppShell>;
  // Keying on workspace:id fully resets all sandbox state — in-flight reads can
  // never repaint into another organization or machine.
  return <AppShell active="Sandboxes" pageTitle="Sandbox" pageKey={`${data.workspace.id}:${id}`}><section className={styles.contentPage}>
    <SandboxWorkbench key={`${data.workspace.id}:${id}`} workspaceId={data.workspace.id} id={id} />
  </section></AppShell>;
}
