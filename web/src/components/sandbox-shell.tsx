"use client";
import Link from "next/link";
import { useState, type ReactNode } from "react";
import { Quicknav } from "./quicknav";
import { SandboxNav } from "./sandbox-nav";
import { SandboxSelect } from "./sandbox-controls";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon } from "./workspace-icon";
import { selectWorkspace, workspaceDisplayName } from "@/lib/workspace-selection";
import styles from "./sandbox-shell.module.css";

// The Sandboxes product shell: the shared quicknav rail plus a compact product
// header. There is deliberately no Home/Sessions sidebar here — conversations
// are where work is asked for, this canvas is for inspecting machines.
export function SandboxShell({ children, actions, tabs }: { children: ReactNode; actions?: ReactNode; tabs?: ReactNode }) {
  const { data } = useWorkspace();
  const [switching, setSwitching] = useState("");
  const [switchError, setSwitchError] = useState("");
  const workspaces = data?.workspaces ?? (data ? [data.workspace] : []);

  async function chooseWorkspace(id: string) {
    if (switching || !id || id === data?.workspace.id) return;
    try {
      setSwitchError("");
      setSwitching(id);
      await selectWorkspace(id);
      // A fresh document drops the previous organisation's in-memory caches
      // and scoped state; drafts stay under their workspace-scoped keys.
      window.location.assign(new URL("/app/sandboxes", window.location.origin).href);
    } catch (cause) {
      setSwitching("");
      setSwitchError(cause instanceof Error ? cause.message : "Couldn’t switch organisations. Try again.");
    }
  }

  return (
    <div className={`dashboard-theme ${styles.shell}`}>
      <a className={styles.skip} href="#sandbox-main">Skip to content</a>
      <Quicknav />
      <SandboxNav />
      <div className={styles.page}>
        <header className={styles.header}>
          <span className={styles.product}>Sandboxes</span>
          {tabs}
          <div className={styles.headerEnd}>
            {actions ? <div className={styles.headerActions}>{actions}</div> : null}
            <SandboxSelect
              label="Organisation"
              searchLabel="Find organisation"
              className={styles.orgSwitch}
              value={switching || (data?.workspace.id ?? "")}
              disabled={!data || !!switching}
              onChange={id => void chooseWorkspace(id)}
              options={workspaces.map(workspace => ({
                value: workspace.id,
                label: `${workspaceDisplayName(workspace.name)}${switching === workspace.id ? " — opening…" : ""}`,
              }))}
            />
            <Link href="/app/settings" prefetch={true} aria-label="Workspace settings" title="Workspace settings" className={styles.settings}>
              <WorkspaceIcon name="gear" width="17" height="17" />
            </Link>
          </div>
        </header>
        {switchError ? <p role="alert" className={styles.switchError}>{switchError}</p> : null}
        <main id="sandbox-main" tabIndex={-1} className={styles.main}>{children}</main>
      </div>
    </div>
  );
}
