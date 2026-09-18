"use client";

import { useEffect, useRef, useState } from "react";
import { SettingsShell } from "./settings-shell";
import { WorkspaceIcon } from "./workspace-icon";
import { useWorkspace } from "./workspace-context";
import { canterFetch } from "@/lib/canter-api";
import styles from "./settings.module.css";

type Secret = { id: string; name: string; purpose: "stored" | "openrouter"; note: string; version: number; updatedBy: string; updatedAt: string; lastUsedAt: string | null };
type SecretState = { secrets: Secret[]; enabled: boolean; canManage: boolean };
type Editing = { mode: "create" } | { mode: "rotate" | "revoke"; secret: Secret };
const date = (value: string) => new Date(value).toLocaleDateString(undefined, { month: "short", day: "numeric" });

export function WorkspaceSecrets() {
  const { data } = useWorkspace();
  const workspace = data?.workspace.id;
  const [state, setState] = useState<SecretState | null>(null);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [editing, setEditing] = useState<Editing | null>(null);
  const [reload, setReload] = useState(0);
  const [notice, setNotice] = useState("");
  useEffect(() => {
    if (!workspace) return;
    let cancelled = false;
    canterFetch<SecretState>(`/workspaces/${encodeURIComponent(workspace)}/secrets`).then(result => { if (!cancelled) { setState(result); setError(""); } }).catch(cause => { if (!cancelled) setError(cause instanceof Error ? cause.message : "Could not load secrets."); });
    return () => { cancelled = true; };
  }, [workspace, reload]);
  const secrets = state?.secrets.filter(secret => `${secret.name} ${secret.note}`.toLowerCase().includes(search.toLowerCase())) ?? [];
  return <SettingsShell active="Secrets" title="Secrets">
    {state && !state.enabled ? <p className={styles.notice}>Secret storage hasn’t been configured on this server yet. Ask your administrator to enable it.</p> : null}
    {state && !state.canManage ? <p className={styles.muted}>Only workspace owners can add, rotate, or remove secrets.</p> : null}
    {error ? <p className={`${styles.notice} ${styles.error}`} role="alert">{error}<button onClick={() => setReload(value => value + 1)}>Retry</button></p> : null}
    {notice ? <p className={styles.notice} role="status">{notice}</p> : null}
    <div className={styles.toolbar}><label className={styles.search}><WorkspaceIcon name="search" width="15" height="15" /><input aria-label="Search secrets" placeholder="Search secrets…" value={search} onChange={event => setSearch(event.target.value)} /></label><button className={styles.primary} disabled={!state?.enabled || !state.canManage} onClick={() => { setNotice(""); setEditing({ mode: "create" }); }}><WorkspaceIcon name="plus" width="14" height="14" />Add secret</button></div>
    <div className={styles.tableWrap}>
      <table className={styles.table}><thead><tr><th>Name</th><th>Use</th><th>Updated</th><th><span className="sr-only">Actions</span></th></tr></thead><tbody>{secrets.map(secret => <tr key={secret.id}>
        <td><strong>{secret.name}</strong>{secret.note ? <small>{secret.note}</small> : <small>Value hidden</small>}</td>
        <td><span className={styles.badge}>{secret.purpose === "openrouter" ? "Hosted agent" : "Stored only"}</span><small>{secret.lastUsedAt ? `Last used ${date(secret.lastUsedAt)}` : "Not used yet"}</small></td>
        <td>{date(secret.updatedAt)}<small>Version {secret.version}{secret.updatedBy === data?.account.id ? " · You" : ""}</small></td>
        <td>{state?.canManage ? <div className={styles.actions}><button className={styles.button} disabled={!state.enabled} aria-label={`Rotate ${secret.name}`} onClick={() => setEditing({ mode: "rotate", secret })}>Rotate</button><button className={styles.button} aria-label={`Remove ${secret.name}`} onClick={() => setEditing({ mode: "revoke", secret })}><WorkspaceIcon name="close" width="13" height="13" /></button></div> : null}</td>
      </tr>)}</tbody></table>
      {!secrets.length ? <div className={styles.empty}><WorkspaceIcon name="lock" width="26" height="26" /><h2>{!state && !error ? "Loading secrets…" : search ? "No matching secrets" : "No workspace secrets yet"}</h2><p>{search ? "Try another name or note." : "Add a credential once. Choose where Canter can use it."}</p></div> : null}
    </div>
    {editing && workspace ? <SecretDialog key={`${workspace}-${editing.mode}`} workspace={workspace} editing={editing} hasOpenRouter={!!state?.secrets.some(secret => secret.purpose === "openrouter")} onClose={() => setEditing(null)} onSaved={() => { setNotice(editing.mode === "revoke" ? "Secret removed from Canter. Revoke the original key with its provider if it should no longer work anywhere." : editing.mode === "rotate" ? "Secret rotated. Future integration requests will use the new value." : "Secret saved. Its value will stay hidden."); setEditing(null); setReload(value => value + 1); }} /> : null}
  </SettingsShell>;
}

function SecretDialog({ workspace, editing, hasOpenRouter, onClose, onSaved }: { workspace: string; editing: Editing; hasOpenRouter: boolean; onClose: () => void; onSaved: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [name, setName] = useState("");
  const [purpose, setPurpose] = useState("stored");
  const [value, setValue] = useState("");
  const [note, setNote] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => { const node = dialog.current; node?.showModal(); node?.querySelector<HTMLInputElement>("input")?.focus(); return () => node?.close(); }, []);
  async function save(event: React.FormEvent) {
    event.preventDefault(); setBusy(true); setError("");
    try {
      await canterFetch(`/workspaces/${encodeURIComponent(workspace)}/secrets${editing.mode === "create" ? "" : `/${encodeURIComponent(editing.secret.id)}`}`, { method: editing.mode === "create" ? "POST" : editing.mode === "rotate" ? "PUT" : "DELETE", body: JSON.stringify(editing.mode === "create" ? { name, purpose, value, note } : editing.mode === "rotate" ? { value, version: editing.secret.version } : { version: editing.secret.version }) });
      setValue(""); onSaved();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Could not save the secret."); setBusy(false); }
  }
  const remove = editing.mode === "revoke";
  return <dialog ref={dialog} className={styles.dialog} aria-labelledby="secret-dialog-title" onCancel={event => { if (busy) event.preventDefault(); else onClose(); }}>
    <header><h2 id="secret-dialog-title">{editing.mode === "create" ? "New workspace secret" : `${remove ? "Remove" : "Rotate"} ${editing.secret.name}`}</h2><button disabled={busy} onClick={onClose} aria-label="Close secret dialog"><WorkspaceIcon name="close" /></button></header>
    <p className={styles.muted}>{remove ? "Canter will delete its stored value and stop using it for future requests. Requests already sent may finish." : "Only workspace owners can manage this value. It won’t be shown again after saving."}</p>
    <form className={styles.form} onSubmit={event => void save(event)}>
      {editing.mode === "create" ? <><label>Secret name<input required autoFocus autoComplete="off" spellCheck={false} maxLength={80} pattern="[A-Z][A-Z0-9_]{1,79}" placeholder="SERVICE_API_KEY" value={name} onChange={event => setName(event.target.value.toUpperCase())} /></label>
        <label>Use this secret for<select value={purpose} onChange={event => setPurpose(event.target.value)}><option value="stored">Store for later</option><option value="openrouter" disabled={hasOpenRouter}>OpenRouter · Canter hosted agent{hasOpenRouter ? " (already connected)" : ""}</option></select><small>{purpose === "openrouter" ? "All workspace users’ hosted conversations will use this key. OpenRouter charges apply to its account. Set a spending limit with OpenRouter before connecting." : "Encrypted storage only. This value won’t be injected into agents, prompts, or apps."}</small></label></> : null}
      {!remove ? <label>{editing.mode === "rotate" ? "New secret value" : "Secret value"}<input required type="password" autoFocus={editing.mode === "rotate"} autoComplete="new-password" spellCheck={false} maxLength={8192} placeholder="Paste your key" value={value} onChange={event => setValue(event.target.value)} /><small>Enter credentials here, never in a conversation.</small></label> : null}
      {editing.mode === "create" ? <label>Note <textarea maxLength={500} placeholder="What is this used for? Don’t include credentials." value={note} onChange={event => setNote(event.target.value)} /><small>Notes and names are visible to workspace members and authorized agents.</small></label> : null}
      {remove && editing.secret.purpose === "openrouter" ? <p className={styles.notice}>Future conversations will return to the server’s default model connection, if one is configured.</p> : null}
      {error ? <p className={`${styles.notice} ${styles.error}`} role="alert">{error}</p> : null}
      <div className={styles.actions}><button type="button" className={styles.button} disabled={busy} onClick={onClose}>Cancel</button><button className={styles.primary} disabled={busy}>{busy ? "Saving…" : remove ? "Remove secret" : editing.mode === "rotate" ? "Replace value" : "Save secret"}</button></div>
    </form>
  </dialog>;
}
