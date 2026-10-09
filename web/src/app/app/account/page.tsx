"use client";

import Link from "next/link";
import { useState } from "react";
import { SettingsShell, SettingRow } from "@/components/settings-shell";
import { CopyValue } from "@/components/copy-value";
import { AccountDeletion } from "@/components/account-deletion";
import { useWorkspace } from "@/components/workspace-context";
import { canterFetch } from "@/lib/canter-api";
import { clearAllOperatorAttachmentDrafts } from "@/lib/operator-attachment-draft";
import { clearOperatorTextDrafts } from "@/lib/operator-draft-storage";
import styles from "@/components/settings.module.css";

export default function AccountPage() {
  const { data, error: loadError } = useWorkspace();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  async function signOut() {
    setBusy(true);
    try {
      await canterFetch("/auth/signout", { method: "POST" });
      try { clearOperatorTextDrafts(sessionStorage); } catch { /* Session storage may be unavailable. */ }
      await clearAllOperatorAttachmentDrafts();
      window.location.replace("/sign-in");
    }
    catch { setError("Could not sign out. Please try again."); setBusy(false); }
  }
  return <SettingsShell active="Profile" title="Profile" description="Your personal account and sign-in session.">
    {error || loadError ? <p className={`${styles.notice} ${styles.error}`} role="alert">{error || String(loadError)}</p> : null}
    <section className={styles.section}><h2>Personal information</h2><div className={styles.card}>
      <SettingRow title="Profile" description="Your account in Canter"><span className={styles.avatar}>{data?.account.email.slice(0, 1).toUpperCase() ?? "…"}</span></SettingRow>
      <SettingRow title="Email" description="The email you use to sign in">{data?.account.email ?? "Loading…"}</SettingRow>
      <SettingRow title="Account ID" description="Use this when contacting support"><CopyValue value={data?.account.id} label="Account ID" /></SettingRow>
    </div></section>
    <section className={styles.section}><h2>Account security</h2><div className={styles.card}><SettingRow title="Sign-in and recovery" description="Passkeys, authenticator apps, recovery codes, and sessions."><Link className={styles.button} href="/app/account/security">Manage security</Link></SettingRow></div></section>
    <section className={styles.section}><h2>Session</h2><div className={styles.card}>
      <SettingRow title="Sign out" description="End this browser’s Canter session."><button className={styles.button} disabled={busy} onClick={() => void signOut()}>{busy ? "Signing out…" : "Sign out"}</button></SettingRow>
      <SettingRow title="Delete account" description="Permanently remove your account and private data."><AccountDeletion disabled={busy || !data?.account.id} /></SettingRow>
    </div></section>
  </SettingsShell>;
}
