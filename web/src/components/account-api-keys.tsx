"use client";

import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { apiKeyExpiryOptions, apiKeysPath, apiKeyResumeID, cancelAPIKeyIntent, storeAPIKeyIntent, takeAPIKeyIntent, type APIKeyInput } from "@/lib/api-key-intent";
import { canterFetch, CanterAPIError } from "@/lib/canter-api";
import { SettingsShell } from "./settings-shell";
import { CanterSelect } from "./canter-select";
import { WorkspaceIcon } from "./workspace-icon";
import { useDialog } from "./use-dialog";
import { useWorkspace } from "./workspace-context";
import keys from "./account-api-keys.module.css";
import styles from "./settings.module.css";

type PersonalAPIKey = {
  id: string;
  name: string;
  prefix: string;
  createdAt: string;
  expiresAt: string;
  lastUsedAt?: string;
  revokedAt?: string;
};

type CreateResponse = { key: PersonalAPIKey; secret: string };
type KeyStatus = "active" | "expired" | "revoked";

const statusOptions = [
  { value: "all", label: "All keys" },
  { value: "active", label: "Active" },
  { value: "expired", label: "Expired" },
  { value: "revoked", label: "Revoked" },
];

function shortDate(value: string) {
  return new Date(value).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

function fullDate(value: string) {
  return new Date(value).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" });
}

function statusOf(key: PersonalAPIKey): KeyStatus {
  if (key.revokedAt) return "revoked";
  if (new Date(key.expiresAt).getTime() <= Date.now()) return "expired";
  return "active";
}

function APIKeyDialog({ title, children, busy, dismissible = true, initialFocus, onClose }: { title: string; children: ReactNode; busy: boolean; dismissible?: boolean; initialFocus?: string; onClose: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const id = useId();
  const dismiss = useDialog(ref, initialFocus);
  const close = () => dismiss(onClose);
  return <dialog ref={ref} className={`${styles.dialog} ${keys.dialog}`} aria-labelledby={id} aria-busy={busy} onCancel={event => { event.preventDefault(); if (!busy && dismissible) close(); }}>
    <header><h2 id={id} tabIndex={-1}>{title}</h2>{dismissible ? <button type="button" disabled={busy} className={keys.iconButton} aria-label="Close dialog" onClick={close}><WorkspaceIcon name="close" /></button> : null}</header>
    {children}
  </dialog>;
}

export function AccountAPIKeys() {
  const accountId = useWorkspace().data?.account.id;
  const [data, setData] = useState<PersonalAPIKey[] | null>(null);
  const [error, setError] = useState("");
  const [name, setName] = useState("");
  const [expiresInDays, setExpiresInDays] = useState(90);
  const [busy, setBusy] = useState(false);
  const [formError, setFormError] = useState("");
  // The revealed secret lives only in component memory, shown once inside the
  // dialog — never storage, URL, logs, or the base page markup.
  const [secret, setSecret] = useState("");
  const [copyStatus, setCopyStatus] = useState<"" | "copied" | "failed">("");
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState("all");
  const [createOpen, setCreateOpen] = useState(false);
  const [revoking, setRevoking] = useState<PersonalAPIKey | null>(null);
  const [revokeBusy, setRevokeBusy] = useState(false);
  const [revokeError, setRevokeError] = useState("");
  const copyButton = useRef<HTMLButtonElement>(null);
  const createTrigger = useRef<HTMLButtonElement>(null);
  const tableViewport = useRef<HTMLDivElement>(null);
  const alive = useRef(true);
  // A monotonically increasing request sequence: a mutation or unmount makes
  // any in-flight load stale so it can never overwrite newer local state.
  const loadVersion = useRef(0);
  const creating = useRef(false);
  const confirmation = useRef<{ id: string | null } | null>(null);
  // undefined: not yet read from the URL; null: absent or already handled.
  const resume = useRef<string | null | undefined>(undefined);

  useEffect(() => {
    alive.current = true;
    function restoreFromConfirmation(event: PageTransitionEvent) {
      if (!event.persisted || !confirmation.current) return;
      if (confirmation.current.id) cancelAPIKeyIntent(confirmation.current.id);
      confirmation.current = null;
      creating.current = false;
      setBusy(false);
    }
    window.addEventListener("pageshow", restoreFromConfirmation);
    return () => {
      alive.current = false;
      loadVersion.current += 1;
      window.removeEventListener("pageshow", restoreFromConfirmation);
    };
  }, []);

  async function load() {
    const version = ++loadVersion.current;
    try {
      const result = await canterFetch<{ keys: PersonalAPIKey[] }>("/me/api-keys");
      if (alive.current && version === loadVersion.current) { setData(result.keys); setError(""); }
    } catch (cause) {
      if (alive.current && version === loadVersion.current) setError(cause instanceof Error ? cause.message : "Could not load API keys.");
    }
  }
  useEffect(() => {
    const timer = window.setTimeout(() => { void load(); }, 0);
    return () => window.clearTimeout(timer);
  }, []);

  useEffect(() => {
    if (secret) copyButton.current?.focus({ preventScroll: true });
  }, [secret]);

  const createKey = useCallback(async (input: APIKeyInput, mayRedirect: boolean) => {
    if (creating.current) return;
    creating.current = true;
    setBusy(true);
    setFormError("");
    let redirecting = false;
    try {
      const result = await canterFetch<CreateResponse>("/me/api-keys", { method: "POST", body: JSON.stringify(input) });
      if (!alive.current) return;
      loadVersion.current += 1;
      setData(list => [result.key, ...(list ?? [])]);
      setSecret(result.secret);
      setCopyStatus("");
      setName("");
      setQuery("");
      setStatus("all");
    } catch (cause) {
      if (!alive.current) return;
      if (cause instanceof CanterAPIError && cause.status === 428) {
        if (mayRedirect) {
          const id = accountId ? storeAPIKeyIntent(input, accountId) : null;
          confirmation.current = { id };
          const next = id ? `${apiKeysPath}?${new URLSearchParams({ resume: id })}` : apiKeysPath;
          window.location.assign(`/sign-in?${new URLSearchParams({ reauth: "1", next })}`);
          redirecting = true;
        } else {
          setFormError("Identity confirmation is required again. Submit to try again.");
        }
      } else {
        setFormError(cause instanceof Error ? cause.message : "Could not create the key.");
      }
    } finally {
      if (!redirecting) {
        creating.current = false;
        if (alive.current) setBusy(false);
      }
    }
  }, [accountId]);

  useEffect(() => {
    if (resume.current === undefined) {
      const url = new URL(window.location.href);
      resume.current = null;
      if (url.searchParams.has("resume")) {
        const id = apiKeyResumeID(url);
        url.searchParams.delete("resume");
        try {
          window.history.replaceState(window.history.state, "", `${url.pathname}${url.search}${url.hash}`);
          resume.current = id;
        } catch {
          // Leave creation manual if the return marker cannot be cleared.
        }
      }
    }
    if (!resume.current || !accountId || data === null || creating.current) return;
    const input = takeAPIKeyIntent(resume.current, accountId);
    resume.current = null;
    if (!input) return;
    setName(input.name);
    setExpiresInDays(input.expiresInDays);
    setCreateOpen(true);
    void createKey(input, false);
  }, [accountId, data, createKey]);

  function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (busy || !name.trim()) return;
    void createKey({ name: name.trim(), expiresInDays }, true);
  }

  async function revoke() {
    const key = revoking;
    if (!key || revokeBusy) return;
    setRevokeBusy(true);
    setRevokeError("");
    try {
      await canterFetch(`/me/api-keys/${encodeURIComponent(key.id)}`, { method: "DELETE" });
      if (!alive.current) return;
      loadVersion.current += 1;
      setData(list => (list ?? []).map(item => item.id === key.id ? { ...item, revokedAt: new Date().toISOString() } : item));
      setRevoking(null);
      // The row's Revoke action is gone now, so return focus to the list.
      requestAnimationFrame(() => tableViewport.current?.focus({ preventScroll: true }));
    } catch (cause) {
      if (alive.current) setRevokeError(cause instanceof Error ? cause.message : "Could not revoke the key.");
    } finally {
      if (alive.current) setRevokeBusy(false);
    }
  }

  async function copySecret() {
    try {
      await navigator.clipboard.writeText(secret);
      setCopyStatus("copied");
    } catch {
      setCopyStatus("failed");
      const selection = window.getSelection();
      const field = document.querySelector<HTMLElement>(`[data-api-key-secret]`);
      if (selection && field) {
        const range = document.createRange();
        range.selectNodeContents(field);
        selection.removeAllRanges();
        selection.addRange(range);
      }
    }
  }

  const normalized = query.trim().toLowerCase();
  const filtered = (data ?? []).filter(key =>
    (status === "all" || statusOf(key) === status) &&
    (!normalized || key.name.toLowerCase().includes(normalized) || key.prefix.toLowerCase().includes(normalized)));

  return <SettingsShell active="API" title="API keys">
    <section className={keys.management} aria-label="Personal API keys">
      <div className={keys.toolbar}>
        <div className={keys.filters}>
          <label className={keys.search}><WorkspaceIcon name="search" width="14" height="14" /><input type="search" value={query} onChange={event => setQuery(event.target.value)} aria-label="Filter keys" placeholder="Filter keys..." /></label>
          <CanterSelect value={status} onChange={setStatus} ariaLabel="Filter by status" options={statusOptions} />
        </div>
        <button className={styles.button} ref={createTrigger} onClick={() => { setFormError(""); setCreateOpen(true); }} disabled={data === null}><WorkspaceIcon name="plus" width="14" height="14" />Create key</button>
      </div>
      {error ? <p className={`${styles.notice} ${styles.error}`} role="alert">{error} <button type="button" onClick={() => void load()}>Retry</button></p> : null}
      <div className={keys.tableViewport} tabIndex={0} role="region" aria-label="API key list" ref={tableViewport}>
        <table className={keys.table}>
          <colgroup><col /><col className={keys.dateCol} /><col className={keys.dateCol} /><col className={keys.dateCol} /><col className={keys.statusCol} /><col className={keys.actionCol} /></colgroup>
          <thead><tr><th scope="col">Key</th><th scope="col">Created</th><th scope="col">Last used</th><th scope="col">Expires</th><th scope="col">Status</th><th scope="col"><span className="sr-only">Actions</span></th></tr></thead>
          <tbody>
            {!data && !error ? <tr><td colSpan={6} className={keys.emptyRow} role="status">Loading API keys…</td></tr> : null}
            {data && data.length > 0 && !filtered.length ? <tr><td colSpan={6} className={keys.emptyRow}>No keys match your filters.<span className={keys.emptyHint}><button type="button" className={keys.clearFilters} onClick={() => { setQuery(""); setStatus("all"); }}>Clear filters</button></span></td></tr> : null}
            {filtered.map(key => { const state = statusOf(key); return <tr key={key.id}>
              <td><span className={keys.keyName}>{key.name}</span><span className={keys.keyPrefix}>{key.prefix}…</span></td>
              <td><time dateTime={key.createdAt} title={fullDate(key.createdAt)}>{shortDate(key.createdAt)}</time></td>
              <td>{key.lastUsedAt ? <time dateTime={key.lastUsedAt} title={fullDate(key.lastUsedAt)}>{shortDate(key.lastUsedAt)}</time> : "Never"}</td>
              <td><time dateTime={key.expiresAt} title={fullDate(key.expiresAt)}>{shortDate(key.expiresAt)}</time></td>
              <td><span className={keys.status} data-status={state}><span className={keys.statusDot} />{state[0].toUpperCase() + state.slice(1)}</span></td>
              <td>{state === "active" ? <button type="button" className={keys.revoke} aria-label={`Revoke ${key.name}`} onClick={() => { setRevoking(key); setRevokeError(""); }}>Revoke</button> : null}</td>
            </tr>; })}
          </tbody>
        </table>
      </div>
    </section>

    {createOpen ? <APIKeyDialog title={secret ? "Your API key" : "Create API key"} busy={busy} dismissible={!secret} initialFocus="input" onClose={() => { setSecret(""); setCopyStatus(""); setCreateOpen(false); }}>
      {secret ? <div className={keys.reveal}>
        <p>Copy this key now. You won’t be able to see it again.</p>
        <code className={keys.secretValue} data-api-key-secret>{secret}</code>
        <div className={keys.actions}>
          <button type="button" ref={copyButton} className={styles.button} onClick={() => void copySecret()}>Copy key</button>
          <button type="button" className={styles.button} onClick={() => { setSecret(""); setCopyStatus(""); setCreateOpen(false); }}>Done</button>
        </div>
        <p className={keys.copyStatus} role="status">{copyStatus === "copied" ? "Key copied" : copyStatus === "failed" ? "Could not copy. Select and copy the key." : ""}</p>
      </div> : <form className={keys.createForm} onSubmit={create}>
        <label className={keys.field}><span>Name</span><input type="text" required maxLength={80} placeholder="e.g. Laptop CLI" value={name} disabled={busy} onChange={event => setName(event.target.value)} /></label>
        <label className={keys.field}><span>Expiration</span><CanterSelect value={String(expiresInDays)} disabled={busy} onChange={value => setExpiresInDays(Number(value))} ariaLabel="Key expiry" options={apiKeyExpiryOptions.map(option => ({ value: String(option.days), label: `Expires in ${option.label}` }))} /></label>
        <p className={keys.fieldNote}>This key can read and write resources wherever your account has access. It cannot manage keys or account security.</p>
        {formError ? <p className={`${styles.notice} ${styles.error}`} role="alert">{formError}</p> : null}
        <div className={keys.actions}>
          <button type="button" className={styles.button} disabled={busy} onClick={() => setCreateOpen(false)}>Cancel</button>
          <button type="submit" className={styles.primary} disabled={busy || !name.trim()}>{busy ? "Creating…" : "Create key"}</button>
        </div>
      </form>}
    </APIKeyDialog> : null}

    {revoking ? <APIKeyDialog title="Revoke API key?" busy={revokeBusy} initialFocus="[data-revoke-cancel]" onClose={() => { setRevoking(null); setRevokeError(""); }}>
      <p className={keys.fieldNote}>“{revoking.name}” will stop working immediately. Tools using this key will need a new one.</p>
      {revokeError ? <p className={`${styles.notice} ${styles.error}`} role="alert">{revokeError}</p> : null}
      <div className={keys.actions}>
        <button type="button" data-revoke-cancel className={styles.button} disabled={revokeBusy} onClick={() => setRevoking(null)}>Cancel</button>
        <button type="button" className={`${styles.button} ${keys.danger}`} disabled={revokeBusy} onClick={() => void revoke()}>{revokeBusy ? "Revoking…" : "Revoke key"}</button>
      </div>
    </APIKeyDialog> : null}
  </SettingsShell>;
}
