"use client";

import type { useReleaseUpdate } from "./use-release-update";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./release-update-notice.module.css";

export function ReleaseUpdateNotice({ update }: { update: ReturnType<typeof useReleaseUpdate> }) {
  if (!update.available) return null;
  return <div className={styles.notice} role="status">
    <span>Update available</span>
    <button type="button" className={styles.refresh} onClick={update.refresh} aria-label="Refresh Canter">Refresh</button>
    <button type="button" className={styles.dismiss} onClick={update.dismiss} aria-label="Dismiss update notice"><WorkspaceIcon name="close" width="13" height="13" /></button>
  </div>;
}
