"use client";
import { useCallback, useEffect, useState, type FormEvent } from "react";
import { SettingsShell, SettingRow } from "@/components/settings-shell";
import { useWorkspace } from "@/components/workspace-context";
import { canterFetch } from "@/lib/canter-api";
import styles from "@/components/settings.module.css";

type MemoryEntry = { id: string; title: string; content: string; revision: number; createdAt: string; updatedAt: string };

export default function MemorySettingsPage() {
  const { data, error } = useWorkspace();
  const writer = data?.workspace.role === "owner" || data?.workspace.role === "operator";
  const [entries, setEntries] = useState<MemoryEntry[] | null>(null);
  const [loadError, setLoadError] = useState("");
  const base = data ? `/workspaces/${encodeURIComponent(data.workspace.id)}/memory` : "";
  const load = useCallback(async () => {
    try { setEntries((await canterFetch<{ memory: MemoryEntry[] }>(base)).memory); }
    catch (cause) { setLoadError(cause instanceof Error ? cause.message : "Couldn’t load organization notes."); }
  }, [base]);
  useEffect(() => {
    if (!base) return;
    let live = true;
    void (async () => {
      try { const memory = (await canterFetch<{ memory: MemoryEntry[] }>(base)).memory; if (live) setEntries(memory); }
      catch (cause) { if (live) setLoadError(cause instanceof Error ? cause.message : "Couldn’t load organization notes."); }
    })();
    return () => { live = false; };
  }, [base]);
  return <SettingsShell active="Memory" title="Memory" description="Shared notes agents see as organization background.">
    {error ? <p className={styles.notice} role="alert">{String(error)}</p> : null}
    {loadError ? <p className={styles.notice} role="alert">{loadError} <button className={styles.button} onClick={() => void load()}>Retry</button></p> : null}
    <section className={styles.section}><h2>Organization notes</h2>
      {!writer ? <p className={styles.muted}>You have read-only access to this organization.</p> : null}
      {writer ? <MemoryEditor onSaved={entry => { setEntries(current => entry ? [entry, ...(current ?? [])] : current); void load(); }} base={base} /> : null}
      <div className={styles.card}>
        {entries === null ? <SettingRow title="Loading…">{" "}</SettingRow> : entries.length === 0 ? <SettingRow title="No notes yet" description="Notes written here appear as shared background in agent conversations.">{" "}</SettingRow> : entries.map(entry => <MemoryRow key={entry.id} entry={entry} base={base} writer={writer} onDeleted={() => void load()} onSaved={entry => setEntries(current => (current ?? []).map(item => item.id === entry.id ? entry : item))} />)}
      </div>
    </section>
  </SettingsShell>;
}

function MemoryEditor({ base, entry, onSaved, onCancel }: { base: string; entry?: MemoryEntry; onSaved: (entry: MemoryEntry | null) => void; onCancel?: () => void }) {
  const [title, setTitle] = useState(entry?.title ?? "");
  const [content, setContent] = useState(entry?.content ?? "");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  async function save(event: FormEvent) {
    event.preventDefault();
    if (pending) return;
    setPending(true); setError("");
    try {
      const saved = await canterFetch<MemoryEntry>(entry ? `${base}/${encodeURIComponent(entry.id)}` : base, { method: entry ? "PATCH" : "POST", body: JSON.stringify({ title, content, ...(entry ? { baseRevision: entry.revision } : {}) }) });
      onSaved(saved);
      if (!entry) { setTitle(""); setContent(""); }
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : "Couldn’t save this note.";
      setError(message.toLowerCase().includes("conflict") ? "Someone else edited this; reload." : message);
    } finally { setPending(false); }
  }
  return <form className={styles.form} onSubmit={save}>
    <label>Title<input required maxLength={120} value={title} onChange={event => setTitle(event.target.value)} placeholder="Deploy process" /></label>
    <label>Note<textarea required maxLength={4000} value={content} onChange={event => setContent(event.target.value)} /></label>
    {error ? <p className={styles.error} role="alert">{error}</p> : null}
    <div className={styles.actions}><button className={styles.primary} disabled={pending || !title.trim() || !content.trim()}>{pending ? "Saving…" : entry ? "Save" : "Add note"}</button>{onCancel ? <button type="button" className={styles.button} onClick={onCancel}>Cancel</button> : null}</div>
  </form>;
}

function MemoryRow({ entry, base, writer, onSaved, onDeleted }: { entry: MemoryEntry; base: string; writer: boolean; onSaved: (entry: MemoryEntry) => void; onDeleted: () => void }) {
  const [editing, setEditing] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  async function remove() {
    if (pending) return;
    setPending(true); setError("");
    try { await canterFetch(`${base}/${encodeURIComponent(entry.id)}`, { method: "DELETE", body: JSON.stringify({ baseRevision: entry.revision }) }); onDeleted(); }
    catch (cause) {
      const message = cause instanceof Error ? cause.message : "Couldn’t delete this note.";
      setError(message.toLowerCase().includes("conflict") ? "Someone else edited this; reload." : message);
    } finally { setPending(false); }
  }
  if (editing) return <SettingRow title="Edit note"><div className={styles.form}><MemoryEditor base={base} entry={entry} onSaved={entry => { if (entry) onSaved(entry); setEditing(false); }} onCancel={() => setEditing(false)} /></div></SettingRow>;
  return <SettingRow title={entry.title} description={entry.content}>{writer ? <span className={styles.actions}>
    <button className={styles.button} onClick={() => { setEditing(true); setError(""); }}>Edit</button>
    {confirm ? <button className={styles.button} disabled={pending} onClick={() => void remove()}>{pending ? "Deleting…" : "Confirm delete"}</button> : <button className={styles.button} onClick={() => setConfirm(true)}>Delete</button>}
    {confirm ? <button className={styles.button} onClick={() => setConfirm(false)}>Cancel</button> : null}
    {error ? <span role="alert">{error}</span> : null}
  </span> : null}</SettingRow>;
}
