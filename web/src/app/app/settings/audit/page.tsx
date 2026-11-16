"use client";
import { useCallback, useEffect, useRef, useState } from "react";
import { SettingsShell } from "@/components/settings-shell";
import { useWorkspace } from "@/components/workspace-context";
import { canterFetch, relativeTime } from "@/lib/canter-api";
import { auditActionPhrase, type AuditEvent } from "@/lib/audit";
import styles from "@/components/settings.module.css";

export default function AuditSettingsPage() {
  const { data, error } = useWorkspace();
  const [events, setEvents] = useState<AuditEvent[] | null>(null);
  const [cursor, setCursor] = useState("");
  const [busy, setBusy] = useState(false);
  const [loadError, setLoadError] = useState("");
  const base = data ? `/workspaces/${encodeURIComponent(data.workspace.id)}/audit` : "";
  const mounted = useRef(false);
  const load = useCallback(async (before: string, append: boolean) => {
    setBusy(true); setLoadError("");
    try {
      const page = await canterFetch<{ events: AuditEvent[]; nextCursor?: string }>(`${base}?limit=50${before ? `&before=${encodeURIComponent(before)}` : ""}`);
      setCursor(page.nextCursor ?? "");
      setEvents(current => append ? [...(current ?? []), ...page.events] : page.events);
    } catch (cause) { setLoadError(cause instanceof Error ? cause.message : "Couldn’t load the audit log."); }
    finally { setBusy(false); }
  }, [base]);
  useEffect(() => { if (base && !mounted.current) { mounted.current = true; void load("", false); } }, [base, load]);
  return <SettingsShell active="Audit log" title="Audit log" description="Who did what in this organization.">
    {error ? <p className={styles.notice} role="alert">{String(error)}</p> : null}
    {loadError ? <p className={styles.notice} role="alert">{loadError} <button className={styles.button} onClick={() => void load("", false)}>Retry</button></p> : null}
    <section className={styles.section}><div className={styles.card}>
      {events === null ? <div className={styles.row}>Loading…</div> : events.length === 0 ? <div className={styles.row}>Nothing recorded yet.</div> : events.map(event => <div className={styles.row} key={event.id}>
        <div className={styles.rowLabel}>
          <h3><strong>{event.actor.displayName || event.actor.id}</strong>{event.actor.kind === "agent" ? <span className={styles.badge}>{event.actor.harness ? `${event.actor.harness} agent` : "agent"}</span> : null} {auditActionPhrase(event.action)} <span className={styles.muted}>{event.subject}</span></h3>
          <p>{event.occurredAt ? (relativeTime(event.occurredAt) === "now" ? "just now" : `${relativeTime(event.occurredAt)} ago`) : ""}</p>
        </div>
      </div>)}
      {cursor ? <div className={styles.row}><button className={styles.button} disabled={busy} onClick={() => void load(cursor, true)}>{busy ? "Loading…" : "Load more"}</button></div> : null}
    </div></section>
  </SettingsShell>;
}
