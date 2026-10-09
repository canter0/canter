"use client";

import Link from "next/link";
import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { canterFetch, CanterAPIError } from "@/lib/canter-api";
import { authenticatePasskey } from "@/lib/passkeys";
import { clearAllOperatorAttachmentDrafts } from "@/lib/operator-attachment-draft";
import { clearOperatorTextDrafts } from "@/lib/operator-draft-storage";
import { useDialog } from "./use-dialog";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./settings.module.css";
import security from "./account-security.module.css";
import deletion from "./account-deletion.module.css";

type DeletionOptions = {
  hasPassword: boolean;
  factor: "mfa" | "email";
  email: string;
  blocked: string;
  recentAuth: boolean;
  hasPasskey: boolean;
  providers: string[];
};

export function AccountDeletion({ disabled }: { disabled: boolean }) {
  const [open, setOpen] = useState(false);
  return <>
    <button type="button" className={`${styles.button} ${deletion.danger}`} disabled={disabled} onClick={() => setOpen(true)}>Delete account</button>
    {open ? <DeletionDialog onClose={() => setOpen(false)} /> : null}
  </>;
}

function DeletionDialog({ onClose }: { onClose: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const busyRef = useRef(false);
  const id = useId();
  const [options, setOptions] = useState<DeletionOptions | null>(null);
  const [phase, setPhase] = useState<"password" | "verify">("password");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const dismiss = useDialog(ref);
  useEffect(() => {
    const controller = new AbortController();
    canterFetch<DeletionOptions>("/auth/account/delete", { signal: controller.signal })
      .then(setOptions)
      .catch(cause => { if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Could not load account deletion. Try again."); });
    return () => controller.abort();
  }, []);
  useEffect(() => {
    if (busy) return;
    (ref.current?.querySelector<HTMLElement>("[data-initial-focus]") ?? ref.current?.querySelector<HTMLElement>("h2"))?.focus();
  }, [phase, busy, options]);

  function close() {
    if (busyRef.current) return;
    void canterFetch("/auth/account/delete", { method: "DELETE" }).catch(() => { /* An unused proof also expires automatically. */ });
    dismiss(onClose);
  }
  async function run(action: () => Promise<void>) {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setError("");
    try { await action(); }
    catch (cause) {
      if (cause instanceof CanterAPIError && cause.status === 428) {
        setOptions(current => current ? { ...current, recentAuth: false } : current);
      }
      if (cause instanceof CanterAPIError && cause.status === 410) setPhase("password");
      setError(cause instanceof Error ? cause.message : "Could not delete your account. Try again.");
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  }
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    void run(async () => {
      if (phase === "password") {
        const next = await canterFetch<{ factor: "mfa" | "email"; email: string }>("/auth/account/delete/start", { method: "POST", body: JSON.stringify({ password: String(form.get("password") ?? "") }) });
        setOptions(current => current ? { ...current, ...next } : current);
        setPhase("verify");
      } else {
        await canterFetch("/auth/account/delete/finish", { method: "POST", body: JSON.stringify({ code: String(form.get("code") ?? "") }) });
        try { clearOperatorTextDrafts(sessionStorage); } catch { /* Session storage may be unavailable. */ }
        await clearAllOperatorAttachmentDrafts();
        // A full navigation discards private workspace and prefetched data.
        window.location.replace("/sign-in?deleted=1");
      }
    });
  }
  const needsPrimary = options && !options.hasPassword && !options.recentAuth;
  return <dialog ref={ref} className={`${security.dialog} ${deletion.dialog}`} aria-labelledby={id} aria-describedby={`${id}-description`} aria-busy={busy} onCancel={event => { event.preventDefault(); close(); }}>
    <header><h2 id={id} tabIndex={-1}>{phase === "verify" ? "Confirm account deletion" : "Delete account?"}</h2><button type="button" className={security.iconButton} disabled={busy} aria-label="Close dialog" onClick={close}><WorkspaceIcon name="close" /></button></header>
    <p id={`${id}-description`} className={security.description}>This permanently deletes your account, sign-in methods, private conversations, and private workspace data. Shared workspace history and billing records may be retained. This cannot be undone.</p>
    {!options && !error ? <p role="status" className={security.description}>Checking your account…</p> : null}
    {options?.blocked ? <p role="alert" className={`${styles.notice} ${styles.error}`}>{options.blocked}</p> : null}
    {options && !options.blocked ? <>
      {needsPrimary && phase === "password" ? <div>
        <p className={security.description}>Confirm your identity with your existing sign-in method, then continue with {options.factor === "mfa" ? "your authenticator" : "an email code"}.</p>
        <div className={security.alternatives}>
          {options.hasPasskey ? <button type="button" className={styles.button} disabled={busy} onClick={() => void run(async () => { await authenticatePasskey("reauth"); setOptions(await canterFetch<DeletionOptions>("/auth/account/delete")); })}>Use a passkey</button> : null}
          {options.providers.map(provider => <a key={provider} href={`/api/canter/auth/oauth/${encodeURIComponent(provider)}?mode=sign-in&next=%2Fapp%2Faccount`}>Sign in with {provider === "google" ? "Google" : "GitHub"} again</a>)}
          {!options.hasPasskey && !options.providers.length ? <Link href="/sign-in?next=%2Fapp%2Faccount">Sign in again</Link> : null}
        </div>
      </div> : <form className={styles.form} onSubmit={submit} key={phase}>
        <fieldset className={security.fields} disabled={busy}>
          {phase === "password" ? <>
            {options.hasPassword ? <label>Current password<input name="password" type="password" autoComplete="current-password" maxLength={2048} required data-initial-focus /></label> : <p className={security.description}>Your recent sign-in confirms your identity.</p>}
            <p className={styles.muted}>Next, confirm with {options.factor === "mfa" ? "your authenticator or a recovery code" : `a code sent to ${options.email}`}.</p>
          </> : <>
            <p className={security.description}>{options.factor === "mfa" ? "Enter a code from your authenticator app, or use an unused recovery code." : `We sent a deletion code to ${options.email}. It expires in 10 minutes.`}</p>
            <label>{options.factor === "mfa" ? "Authenticator or recovery code" : "Email verification code"}<input name="code" type="text" autoComplete="one-time-code" inputMode={options.factor === "email" ? "numeric" : undefined} pattern={options.factor === "email" ? "[0-9]{6}" : undefined} maxLength={options.factor === "email" ? 6 : 100} required data-initial-focus /></label>
          </>}
          <div className={styles.actions}>
            <button type="button" className={styles.button} onClick={close}>Cancel</button>
            <button className={phase === "verify" ? deletion.confirm : styles.primary}>{busy ? phase === "verify" ? "Deleting…" : "Verifying…" : phase === "verify" ? "Delete account" : "Continue"}</button>
          </div>
          {phase === "verify" ? <button type="button" className={deletion.restart} onClick={() => { setPhase("password"); setError(""); }}>Start again</button> : null}
        </fieldset>
      </form>}
    </> : null}
    {error ? <p role="alert" className={`${styles.notice} ${styles.error} ${security.dialogError}`}>{error}</p> : null}
    {!options || options.blocked || needsPrimary ? <div className={security.confirmActions}>
      <button type="button" className={styles.button} disabled={busy} onClick={close}>Cancel</button>
      {!options || options.blocked ? <button type="button" className={styles.button} disabled={busy} onClick={() => void run(async () => { setOptions(await canterFetch<DeletionOptions>("/auth/account/delete")); })}>Try again</button> : null}
    </div> : null}
  </dialog>;
}
