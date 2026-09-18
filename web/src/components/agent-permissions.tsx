"use client";

import { useId } from "react";
import type { Authority } from "@/lib/canter-api";
import styles from "./agent-permissions.module.css";

export const readAuthority: Authority = { inspect: true, draft: false, applyMode: "never" };
export const writeAuthority: Authority = { inspect: true, draft: true, applyMode: "automatic" };

export function AgentPermissions({ value, onChange, disabled = false }: { value: Authority; onChange: (value: Authority) => void; disabled?: boolean }) {
  const id = useId();
  return <fieldset className={styles.fieldset} disabled={disabled}>
    <legend>Permissions</legend>
    <div className={styles.choices}>
      <label data-selected={!value.draft}><input type="radio" name={id} checked={!value.draft} onChange={() => onChange(readAuthority)} /><span><strong>Read</strong><small>Read across your workspace.</small></span></label>
      <label data-selected={value.draft}><input type="radio" name={id} checked={value.draft} onChange={() => onChange(writeAuthority)} /><span><strong>Write</strong><small>Read and make changes.</small></span></label>
    </div>
    <p>{value.draft ? value.applyMode === "automatic" ? "Can read, write, and deploy across your workspace without asking each time." : value.applyMode === "never" ? "Can read and prepare changes, but cannot apply them." : "Can read and write across your workspace. Deployments require approval." : "Can read across your workspace, without changing apps or configuration."}</p>
    {value.draft ? <details className={styles.customize}><summary>Customize</summary><label>Apply changes<select aria-label="Apply changes" value={value.applyMode} onChange={event => onChange({ ...value, applyMode: event.target.value })}><option value="automatic">Without asking</option><option value="human-approval-required">Ask for approval</option><option value="never">Never</option></select></label></details> : null}
  </fieldset>;
}
