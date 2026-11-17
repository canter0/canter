"use client";

import { useState } from "react";
import type { Workspace } from "@/lib/canter-api";
import { selectWorkspace, workspaceDisplayName } from "@/lib/workspace-selection";
import styles from "./workspace.module.css";

export function JobLinkWorkspace({ id, memberships }: { id: string; memberships?: Workspace[] }) {
  const target = memberships?.find(workspace => workspace.id === id);
  const [switching, setSwitching] = useState(false);
  const [error, setError] = useState("");

  async function open() {
    if (!target || switching) return;
    setSwitching(true);
    setError("");
    try {
      await selectWorkspace(target.id);
      window.location.reload();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Couldn’t open this organization.");
      setSwitching(false);
    }
  }

  return <div className={styles.emptyState}>
    <p>{target ? "This job link belongs to another organization." : "This job is unavailable in your organizations."}</p>
    {target ? <button className={styles.secondaryButton} type="button" disabled={switching} onClick={() => void open()}>
      {switching ? "Opening…" : `Open in ${workspaceDisplayName(target.name)}`}
    </button> : null}
    {error ? <p role="alert" className={styles.error}>{error}</p> : null}
  </div>;
}
