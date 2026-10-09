"use client";

import Link from "next/link";
import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { SettingsShell, SettingRow } from "./settings-shell";
import { WorkspaceIcon } from "./workspace-icon";
import { canterFetch, CanterAPIError } from "@/lib/canter-api";
import { registerPasskey, authenticatePasskey } from "@/lib/passkeys";
import styles from "./settings.module.css";
import security from "./account-security.module.css";
import { useDialog } from "./use-dialog";
import { MotionPresence } from "./motion-presence";

type Passkey = { id: string; name: string; createdAt: string; lastUsedAt: string | null };
type Security = {
  providers: string[];
  totp: boolean;
  totpAvailable: boolean;
  passkeysAvailable: boolean;
  hasPassword: boolean;
  recoveryCodesRemaining: number;
  recentAuth: boolean;
  passkeys: Passkey[];
  sessions: { id: string; userAgent: string; ip: string; current: boolean; lastSeenAt: string; expiresAt: string }[];
  events: { action: string; at: string }[];
};
type ActionDialog =
  | { kind: "password" | "add-passkey" | "setup" | "replace-codes" }
  | { kind: "rename"; passkey: Passkey }
  | { kind: "remove"; title: string; description: string; route: string }
  | { kind: "codes"; codes: string[] };

export function AccountSecurity() {
  const [data, setData] = useState<Security | null>(null);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [dialog, setDialog] = useState<ActionDialog | null>(null);
  const [showReauth, setShowReauth] = useState(false);
  const [setup, setSetup] = useState<{ secret: string; qr: string } | null>(null);
  // Keep interrupted changes only in memory; canceling the dialog discards them.
  const pendingChange = useRef<(() => Promise<void>) | null>(null);
  const returnFocus = useRef<HTMLElement | null>(null);
  const passkeyButton = useRef<HTMLButtonElement>(null);
  const authenticatorActions = useRef<HTMLDivElement>(null);
  const returnToAuthenticator = useRef(false);
  useEffect(() => {
    if (!dialog && !busy && returnFocus.current) {
      const fallback = returnToAuthenticator.current ? authenticatorActions.current?.querySelector("button") : passkeyButton.current;
      const target = returnFocus.current.isConnected ? returnFocus.current : fallback;
      target?.focus({ preventScroll: true });
      returnFocus.current = null;
    }
  }, [dialog, busy]);

  const reload = useCallback(async () => {
    const next = await canterFetch<Security>("/auth/security");
    setData(next);
    return next;
  }, []);
  useEffect(() => {
    let active = true;
    canterFetch<Security>("/auth/security").then(value => {
      if (active) setData(value);
    }).catch(cause => {
      if (active) setError(cause instanceof CanterAPIError && cause.status === 404
        ? "Account security is temporarily unavailable. Please try again."
        : cause instanceof Error ? cause.message : "Could not load security settings.");
    });
    return () => { active = false; };
  }, []);

  function closeDialog() {
    setDialog(null);
    setShowReauth(false);
    setSetup(null);
    pendingChange.current = null;
    setError("");
  }
  async function run(action: () => Promise<void>, canReauthenticate = true) {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setError("");
    setMessage("");
    try {
      try {
        await action();
      } catch (cause) {
        if (canReauthenticate && cause instanceof CanterAPIError && cause.status === 428) {
          pendingChange.current = action;
          setShowReauth(true);
        } else {
          setError(cause instanceof Error ? cause.message : "Could not update account security.");
        }
        return;
      }
      // A refresh failure must never replay a completed credential change.
      try { await reload(); } catch { setError("Could not refresh security settings. Reload this page to see the latest status."); }
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  }
  function mutate(route: string, body: Record<string, string> = {}, method = "POST") {
    return canterFetch<{ recoveryCodes?: string[] }>(`/auth/security/${route}`, { method, body: JSON.stringify(body) });
  }
  async function startSetup() {
    setSetup(await canterFetch("/auth/security/totp/start", { method: "POST", body: "{}" }));
  }
  function openDialog(next: ActionDialog) {
    if (busyRef.current) return;
    returnFocus.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    returnToAuthenticator.current = next.kind === "setup" || (next.kind === "remove" && next.route === "totp");
    setDialog(next);
    setSetup(null);
    setShowReauth(false);
    pendingChange.current = null;
    void run(async () => {
      // Check when the action begins, since an idle page may have stale auth state.
      const current = await reload();
      if (!current.recentAuth) {
        setShowReauth(true);
        pendingChange.current = next.kind === "setup" ? startSetup : null;
      } else if (next.kind === "setup") {
        await startSetup();
      }
    });
  }
  function submit(event: FormEvent<HTMLFormElement>, action: (form: FormData) => Promise<void>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    void run(() => action(form));
  }
  function verify(action: () => Promise<unknown>) {
    void run(async () => {
      await action();
      setShowReauth(false);
      const resume = pendingChange.current;
      pendingChange.current = null;
      await resume?.();
    }, false);
  }
  function saved(text: string) { closeDialog(); setMessage(text); }

  const title = showReauth ? "Confirm your identity"
    : dialog?.kind === "password" ? data?.hasPassword ? "Change password" : "Add a password"
    : dialog?.kind === "add-passkey" ? "Add a passkey"
    : dialog?.kind === "rename" ? "Rename passkey"
    : dialog?.kind === "remove" ? dialog.title
    : dialog?.kind === "replace-codes" ? "Replace recovery codes?"
    : dialog?.kind === "codes" ? "Save your recovery codes"
    : "Set up multi-factor authentication";

  return <SettingsShell active="Security" title="Security" description="Manage your sign-in methods, recovery options, and active sessions.">
    {error && !dialog ? <p role="alert" className={`${styles.notice} ${styles.error}`}>{error}{!data ? <button disabled={busy} onClick={() => void run(async () => { await reload(); }, false)}>Try again</button> : null}</p> : null}
    {message ? <p role="status" className={styles.notice}>{message}</p> : null}
    {!data ? !error ? <p role="status">Loading account security…</p> : null : <>
      <section className={styles.section}>
        <h2>Password</h2>
        <div className={styles.card}>
          <SettingRow title={data.hasPassword ? "Password is set" : "Add a password"} description="Use a unique password or passphrase to sign in.">
            <button className={styles.button} disabled={busy} onClick={() => openDialog({ kind: "password" })}>{data.hasPassword ? "Change password" : "Add password"}</button>
          </SettingRow>
        </div>
      </section>
      <section className={styles.section}>
        <h2>Multi-factor authentication</h2>
        <div className={styles.card}>
          <SettingRow title="Authenticator app" description={data.totp ? "An extra sign-in code from your authenticator protects your account." : "Add an extra layer of security with codes from your authenticator app."}>
            <div ref={authenticatorActions} className={security.rowActions}>
              {data.totp ? <><span className={styles.badge}>Enabled</span><button className={styles.button} disabled={busy} onClick={() => openDialog({ kind: "remove", title: "Remove authenticator?", description: "You will no longer need an authenticator code to sign in. Its recovery codes will also stop working.", route: "totp" })}>Remove</button></>
                : <button className={styles.button} disabled={busy || !data.totpAvailable} onClick={() => openDialog({ kind: "setup" })}>Set up</button>}
            </div>
          </SettingRow>
          {!data.totpAvailable && !data.totp ? <p className={security.availability}>Authenticator setup is temporarily unavailable.</p> : null}
          {data.totp ? <SettingRow title="Recovery codes" description={`${data.recoveryCodesRemaining} unused codes for when you cannot access your authenticator.`}>
            <button disabled={busy} className={styles.button} onClick={() => openDialog({ kind: "replace-codes" })}>Generate new codes</button>
          </SettingRow> : null}
        </div>
      </section>
      <section className={styles.section}>
        <h2>Passkeys</h2>
        <div className={styles.card}>
          <SettingRow title="Sign in with your device" description="Use Touch ID, Face ID, your password manager, or a security key.">
            <button ref={passkeyButton} className={styles.button} disabled={busy || !data.passkeysAvailable} onClick={() => openDialog({ kind: "add-passkey" })}>Add passkey</button>
          </SettingRow>
          {data.passkeys.map(passkey => <PasskeyRow key={passkey.id} passkey={passkey} disabled={busy} onRename={() => openDialog({ kind: "rename", passkey })} onRemove={() => openDialog({ kind: "remove", title: "Remove passkey?", description: `“${passkey.name}” will no longer be able to sign in to Canter.`, route: `passkeys/${encodeURIComponent(passkey.id)}` })} />)}
          {!data.passkeysAvailable ? <p className={security.availability}>Passkey setup is temporarily unavailable.</p> : null}
        </div>
      </section>
      <section className={styles.section}>
        <h2>Active sessions</h2>
        <div className={styles.card}>
          {data.sessions.map(session => <SettingRow key={session.id} title={session.current ? "This browser" : session.userAgent || "Browser session"} description={`${session.ip || "Location unavailable"} · Active ${new Date(session.lastSeenAt).toLocaleString()}`}>
            {session.current ? <span className={styles.badge}>Current</span> : <button disabled={busy} className={styles.button} onClick={() => void run(async () => { await mutate(`sessions/${session.id}`, {}, "DELETE"); setMessage("Session signed out."); }, false)}>Sign out</button>}
          </SettingRow>)}
          <SettingRow title="Sign out other sessions"><button className={styles.button} disabled={busy || data.sessions.length < 2} onClick={() => void run(async () => { await mutate("sessions/others", {}, "DELETE"); setMessage("Other sessions signed out."); }, false)}>Sign out others</button></SettingRow>
        </div>
      </section>
      <section className={styles.section}>
        <h2>Recent security activity</h2>
        <div className={styles.card}>{data.events.length ? data.events.map((event, index) => <SettingRow key={`${event.at}-${index}`} title={event.action}><time dateTime={event.at}>{new Date(event.at).toLocaleString()}</time></SettingRow>) : <p className={security.availability}>No recent security events.</p>}</div>
      </section>
      <Link className="text-xs underline" href="/app/account/connections">Manage connected sign-in accounts</Link>
      {dialog ? <SecurityDialog title={title} busy={busy} dismissible={dialog.kind !== "codes"} focusKey={`${showReauth ? "reauth" : dialog.kind}:${dialog.kind === "setup" && !!setup}`} onClose={closeDialog}>
        {showReauth ? <div>
          <p className={security.description}>Confirm it is you to continue with this change.</p>
          {data.hasPassword || data.totp ? <form className={styles.form} onSubmit={event => { event.preventDefault(); const form = new FormData(event.currentTarget); verify(() => mutate("reauth", { password: String(form.get("password") ?? ""), code: String(form.get("code") ?? "") })); }}>
            <fieldset disabled={busy} className={security.fields}>
              {data.hasPassword ? <label>Current password<input name="password" type="password" autoComplete="current-password" required data-initial-focus /></label> : null}
              {data.totp ? <label>Authenticator or recovery code<input name="code" autoComplete="one-time-code" required data-initial-focus={!data.hasPassword || undefined} /></label> : null}
              <div className={styles.actions}><button type="button" className={styles.button} onClick={closeDialog}>Cancel</button><button className={styles.primary}>{busy ? "Verifying…" : "Continue"}</button></div>
            </fieldset>
          </form> : null}
          {data.passkeys.length || data.providers.length ? <div className={security.alternatives}>
            {data.passkeys.length ? <button disabled={busy} className={styles.button} onClick={() => verify(() => authenticatePasskey("reauth"))}>Use a passkey</button> : null}
            {data.providers.map(provider => <a key={provider} href={`/api/canter/auth/oauth/${provider}?mode=sign-in&next=%2Fapp%2Faccount%2Fsecurity`}>Sign in with {provider === "google" ? "Google" : "GitHub"} again</a>)}
          </div> : null}
        </div> : null}
        {/* Keep a form's values while a server-required identity check interrupts it. */}
        <div hidden={showReauth}>
          {dialog.kind === "password" ? <form className={styles.form} onSubmit={event => submit(event, async form => { await mutate("password", { password: String(form.get("password")) }); saved("Password updated. Your other sessions have been signed out."); })}>
            <fieldset disabled={busy} className={security.fields}>
              <label>New password<input name="password" type="password" autoComplete="new-password" maxLength={2048} required data-initial-focus /><small>At least 15 characters. Use a unique password or passphrase.</small></label>
              <p className={styles.muted}>Changing your password signs out your other sessions.</p>
              <div className={styles.actions}><button type="button" className={styles.button} onClick={closeDialog}>Cancel</button><button className={styles.primary}>{busy ? "Saving…" : "Save password"}</button></div>
            </fieldset>
          </form> : null}
          {dialog.kind === "add-passkey" || dialog.kind === "rename" ? <form className={styles.form} onSubmit={event => submit(event, async form => {
            const name = String(form.get("name")).trim();
            if (!name) throw new Error("Enter a name for this passkey.");
            if (dialog.kind === "rename") { await mutate(`passkeys/${encodeURIComponent(dialog.passkey.id)}`, { name }, "PATCH"); saved("Passkey renamed."); }
            else { await registerPasskey(name); saved("Passkey added. You can now use it to sign in."); }
          })}>
            <fieldset disabled={busy} className={security.fields}>
              <label>Passkey name<input name="name" defaultValue={dialog.kind === "rename" ? dialog.passkey.name : ""} placeholder="MacBook or security key" maxLength={80} required data-initial-focus onFocus={event => { if (dialog.kind === "rename") event.currentTarget.select(); }} /></label>
              {dialog.kind === "add-passkey" ? <p className={styles.muted}>Your device will guide you through setup. Canter never receives your biometric data.</p> : null}
              <div className={styles.actions}><button type="button" className={styles.button} onClick={closeDialog}>Cancel</button><button className={styles.primary}>{busy ? "Saving…" : dialog.kind === "rename" ? "Save name" : "Continue"}</button></div>
            </fieldset>
          </form> : null}
          {dialog.kind === "setup" ? setup ? <>
            <p className={security.description}>Scan this QR code with your authenticator app, then enter its six-digit code. Setup expires after 10 minutes.</p>
            <div className={security.setup}>
              {/* Generated by Canter; the secret never goes to an image service. */}
              {/* eslint-disable-next-line @next/next/no-img-element */}
              <img src={setup.qr} alt="Authenticator setup QR code" width={192} height={192} className={security.qr} />
              <details className={security.manualKey}><summary>Cannot scan the code?</summary><p>Enter this setup key in your authenticator:</p><code>{setup.secret}</code></details>
            </div>
            <form className={styles.form} onSubmit={event => submit(event, async form => { const result = await mutate("totp/confirm", { code: String(form.get("code")) }); setSetup(null); setDialog({ kind: "codes", codes: result.recoveryCodes ?? [] }); setMessage("Multi-factor authentication enabled."); })}>
              <fieldset disabled={busy} className={security.fields}>
                <label>Authentication code<input name="code" inputMode="numeric" autoComplete="one-time-code" pattern="[0-9]{6}" maxLength={6} required data-initial-focus /></label>
                <div className={styles.actions}><button type="button" className={styles.button} onClick={closeDialog}>Cancel</button><button className={styles.primary}>{busy ? "Enabling…" : "Enable MFA"}</button></div>
              </fieldset>
            </form>
          </> : <div className={security.preparing}><p role="status">{busy ? "Preparing authenticator setup…" : "Start authenticator setup to get your QR code."}</p>{!busy ? <button className={styles.button} onClick={() => void run(startSetup)}>Try again</button> : null}</div> : null}
          {dialog.kind === "remove" || dialog.kind === "replace-codes" ? <>
            <p className={security.description}>{dialog.kind === "remove" ? dialog.description : "Create a new set if your recovery codes are lost or exposed. Every old code will stop working immediately. Your authenticator will still work."}</p>
            <p className={styles.muted}>Your other sessions will be signed out.</p>
            <div className={security.confirmActions}><button disabled={busy} className={styles.button} data-initial-focus onClick={closeDialog}>Cancel</button><button disabled={busy} className={styles.primary} onClick={() => void run(async () => {
              if (dialog.kind === "remove") { await mutate(dialog.route, {}, "DELETE"); saved("Sign-in method removed."); }
              else { const result = await mutate("recovery-codes"); setDialog({ kind: "codes", codes: result.recoveryCodes ?? [] }); setMessage("New recovery codes created. Your old codes no longer work."); }
            })}>{busy ? "Saving…" : dialog.kind === "remove" ? "Remove" : "Replace codes"}</button></div>
          </> : null}
          {dialog.kind === "codes" ? <>
            <p className={security.description}>Each code can replace an authenticator code once. Save them somewhere private. Canter cannot show these codes again.</p>
            <ul className={security.codes} aria-label="Single-use recovery codes">{dialog.codes.map(code => <li key={code}>{code}</li>)}</ul>
            <div className={security.recoveryActions}><button className={styles.primary} onClick={() => downloadRecoveryCodes(dialog.codes)}><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true"><path d="M12 3v12m-5-5 5 5 5-5M4 16v5h16v-5" /></svg>Download codes</button><button className={styles.button} disabled={busy} onClick={closeDialog}>I saved these codes</button></div>
          </> : null}
        </div>
        {error ? <p role="alert" className={`${styles.notice} ${styles.error} ${security.dialogError}`}>{error}</p> : null}
      </SecurityDialog> : null}
    </>}
  </SettingsShell>;
}

function SecurityDialog({ title, children, busy, dismissible, focusKey, onClose }: { title: string; children: ReactNode; busy: boolean; dismissible: boolean; focusKey: string; onClose: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const id = useId();
  const dismiss = useDialog(ref);
  const close = () => dismiss(onClose);
  useEffect(() => {
    if (busy) return;
    const element = ref.current;
    const target = Array.from(element?.querySelectorAll<HTMLElement>("[data-initial-focus]") ?? []).find(item => !item.closest("[hidden]"));
    (target ?? element?.querySelector<HTMLElement>("h2"))?.focus();
  }, [focusKey, busy]);
  return <dialog ref={ref} className={security.dialog} aria-labelledby={id} aria-busy={busy} onCancel={event => { event.preventDefault(); if (!busy && dismissible) close(); }}>
    <header><h2 id={id} tabIndex={-1}>{title}</h2>{dismissible ? <button type="button" disabled={busy} className={security.iconButton} aria-label="Close dialog" onClick={close}><WorkspaceIcon name="close" /></button> : null}</header>
    {children}
  </dialog>;
}

function PasskeyRow({ passkey, disabled, onRename, onRemove }: { passkey: Passkey; disabled: boolean; onRename: () => void; onRemove: () => void }) {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const id = useId();
  useEffect(() => {
    if (!open) return;
    root.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus();
    const outside = (event: PointerEvent) => { if (event.target instanceof Node && !root.current?.contains(event.target)) setOpen(false); };
    document.addEventListener("pointerdown", outside);
    return () => document.removeEventListener("pointerdown", outside);
  }, [open]);
  function select(action: () => void) { setOpen(false); trigger.current?.focus(); action(); }
  return <div className={security.passkeyRow}>
    <span className={security.passkeyName}><WorkspaceIcon name="lock" /><span title={passkey.name}>{passkey.name}</span></span>
    <div ref={root} className={security.menuAnchor} onKeyDown={event => {
      if (event.key === "Escape" || event.key === "Tab") { if (event.key === "Escape") event.preventDefault(); setOpen(false); trigger.current?.focus(); }
      if (["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) {
        event.preventDefault();
        if (!open) { setOpen(true); return; }
        const items = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>('[role="menuitem"]'));
        const index = items.indexOf(document.activeElement as HTMLButtonElement);
        items[event.key === "Home" ? 0 : event.key === "End" ? items.length - 1 : (index + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length]?.focus();
      }
    }}>
      <button ref={trigger} type="button" disabled={disabled} className={security.iconButton} aria-label={`Options for ${passkey.name}`} aria-haspopup="menu" aria-expanded={open} aria-controls={open ? id : undefined} onClick={() => setOpen(value => !value)}><WorkspaceIcon name="more" /></button>
      <MotionPresence open={open}><div id={id} className={security.menu} role="menu" aria-label={`Passkey options for ${passkey.name}`}><button role="menuitem" onClick={() => select(onRename)}><WorkspaceIcon name="edit" width="15" height="15" />Rename</button><button role="menuitem" className={security.danger} onClick={() => select(onRemove)}><WorkspaceIcon name="trash" width="15" height="15" />Remove</button></div></MotionPresence>
    </div>
  </div>;
}

function downloadRecoveryCodes(codes: string[]) {
  const blob = new Blob(["Canter recovery codes — keep private. Each code works once.\n\n" + codes.join("\n")], { type: "text/plain" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = "canter-recovery-codes.txt";
  document.body.appendChild(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
