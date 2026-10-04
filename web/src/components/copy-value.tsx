"use client";

import { useState } from "react";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./settings.module.css";

export function CopyValue({ value, label }: { value?: string; label: string }) {
  const [status, setStatus] = useState("");
  return <div className={styles.copyValue}><code>{value ?? "—"}</code>{value ? <button type="button" className={styles.copyButton} aria-label={`Copy ${label}`} title={`Copy ${label}`} onClick={async () => {
    try { await navigator.clipboard.writeText(value); setStatus(`${label} copied`); }
    catch { setStatus("Couldn’t copy. Select and copy the value above."); }
  }}><WorkspaceIcon name="copy" width="15" height="15" /></button> : null}<span className={styles.copyStatus} role="status">{status}</span></div>;
}
