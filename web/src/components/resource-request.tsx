"use client";

import { useId, useState, type FormEvent } from "react";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./resource-request.module.css";

export type ResourceKind = "compute" | "storage";
export function ResourceRequest({ kind, busy, onContinue, onGitHub }: { kind: ResourceKind; busy: boolean; onContinue: (message: string) => Promise<boolean>; onGitHub: () => void }) {
  const id = useId();
  const [name, setName] = useState("");
  const [purpose, setPurpose] = useState("");
  const [size, setSize] = useState("small");
  const [access, setAccess] = useState("private");
  const [submitted, setSubmitted] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const compute = kind === "compute";
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy || submitting) return;
    setSubmitting(true);
    try {
      const message = compute
        ? `Help me plan a VPS named ${name.trim()}. Intended workload: ${purpose.trim()}. Starting size preference: ${size}. Check what Canter supports and explain the next available step. This is a planning request, not authorization to provision or incur charges.`
        : `Help me plan a storage bucket named ${name.trim()}. Intended use: ${purpose.trim()}. Access preference: ${access}. Check what Canter supports and explain the next available step. This is a planning request, not authorization to create a bucket or incur charges.`;
      if (await onContinue(message)) setSubmitted(true);
    } finally { setSubmitting(false); }
  }
  return <section className={styles.card} aria-label={compute ? "VPS setup" : "Storage bucket setup"}>
    <header><span className={styles.icon}><WorkspaceIcon name={compute ? "apps" : "folder"} /></span><div><h2>{compute ? "Plan a VPS" : "Plan a storage bucket"}</h2><p>{compute ? "Start with what you want to run." : "Give your files a home."}</p></div><span className={styles.badge}>Planning</span></header>
    <form onSubmit={event => void submit(event)}>
      <label htmlFor={`${id}-name`}>{compute ? "Server name" : "Bucket name"}</label>
      <input id={`${id}-name`} value={name} onChange={event => { setName(event.target.value); setSubmitted(false); }} placeholder={compute ? "my-server" : "app-uploads"} pattern="[a-z0-9][a-z0-9-]{1,61}[a-z0-9]" title="3–63 lowercase letters, numbers or hyphens; start and end with a letter or number." required maxLength={63} autoComplete="off" />
      <label htmlFor={`${id}-purpose`}>{compute ? "What will it run?" : "What will you store?"}</label>
      <input id={`${id}-purpose`} value={purpose} onChange={event => { setPurpose(event.target.value); setSubmitted(false); }} placeholder={compute ? "A website, an API, a background worker…" : "Photos, backups, app uploads…"} required maxLength={400} />
      {compute ? <fieldset><legend>Starting size</legend><div className={styles.options}>{([['small', 'Small', 'A lightweight workload'], ['medium', 'Medium', 'Room for a growing app'], ['large', 'Large', 'A heavier workload']] as const).map(([value, label, hint]) => <label key={value} className={styles.option} data-selected={size === value}><input type="radio" name={`${id}-size`} value={value} checked={size === value} onChange={() => { setSize(value); setSubmitted(false); }} /><strong>{label}</strong><small>{hint}</small></label>)}</div></fieldset> : <fieldset><legend>File access</legend><div className={styles.options}>{([['private', 'Private', 'Only authorized access'], ['public-read', 'Public read', 'Anyone could read files']] as const).map(([value, label, hint]) => <label key={value} className={styles.option} data-selected={access === value}><input type="radio" name={`${id}-access`} value={value} checked={access === value} onChange={() => { setAccess(value); setSubmitted(false); }} /><strong>{label}</strong><small>{hint}</small></label>)}</div></fieldset>}
      <p className={styles.limit}>{compute ? "Canter currently provisions compute as part of an app deployment. Standalone VPS creation isn’t available yet. Size is a preference; specifications and pricing have not been quoted." : "Standalone bucket creation isn’t available yet. These details help plan your request; no bucket or access permissions will be created."}</p>
      <footer><button type="submit" className={styles.primary} disabled={busy || submitting || submitted}>{submitting ? "Sending…" : submitted ? "Request sent" : "Discuss with Canter"}<WorkspaceIcon name="arrow" width="15" height="15" /></button>{compute ? <button type="button" className={styles.secondary} onClick={onGitHub}>Deploy from GitHub</button> : null}</footer>
      {submitted ? <p className={styles.status} role="status">Your requirements were sent to the conversation. Nothing has been provisioned.</p> : null}
    </form>
  </section>;
}
