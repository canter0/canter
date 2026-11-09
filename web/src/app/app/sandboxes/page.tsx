"use client";

import Link from "next/link";
import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { useDialog } from "@/components/use-dialog";
import { useRequestScope } from "@/components/use-request-scope";
import dialogStyles from "@/components/agent-connection-dialog.module.css";
import { AppShell } from "@/components/app-shell";
import { useWorkspace } from "@/components/workspace-context";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { canterFetch, relativeTime, type Workspace } from "@/lib/canter-api";
import { parseSandboxLabels } from "@/lib/sandbox-client";
import { formatMiB, freeHoursText, idleChoiceText, sandboxDesktopEligible, sandboxHourlyCents, sandboxPhaseText, type SandboxListResponse } from "@/lib/sandboxes";
import styles from "@/components/workspace.module.css";

export default function SandboxesPage() {
  const { data, error, loading } = useWorkspace();
  if (!data) return <AppShell active="Sandboxes" pageTitle="Sandboxes"><section className={styles.contentPage}>
    <p role={error ? "alert" : "status"}>{loading ? "Loading sandboxes…" : "Couldn’t load this organization."}</p>
  </section></AppShell>;
  return <SandboxList key={data.workspace.id} workspace={data.workspace} />;
}

function SandboxList({ workspace }: { workspace: Workspace }) {
  const { signal: requestSignal } = useRequestScope();
  const writer = workspace.role === "owner" || workspace.role === "operator";
  const [result, setResult] = useState<SandboxListResponse | null>(null);
  const [loadError, setLoadError] = useState("");
  const [dialog, setDialog] = useState(false);
  const [draft, setDraft] = useState({ name: "", size: "free-1x1", idle: "-1", labels: "", key: "", desktop: false });
  const [createError, setCreateError] = useState("");
  const [creating, setCreating] = useState(false);
  const [submitted, setSubmitted] = useState<string | null>(null);
  const loadSequence = useRef(0);
  const openDialog = useCallback(() => {
    setDraft({ name: "", size: result?.sizes?.[0]?.id ?? "free-1x1", idle: "-1", labels: "", key: crypto.randomUUID(), desktop: false });
    setCreateError("");
    setSubmitted(null);
    setDialog(true);
  }, [result]);
  const sizesReady = result?.enabled && (result?.sizes?.length ?? 0) > 0;
  const load = useCallback(async () => {
    const signal = requestSignal();
    if (signal.aborted) return;
    const sequence = ++loadSequence.current;
    try {
      const next = await canterFetch<SandboxListResponse>(`/workspaces/${encodeURIComponent(workspace.id)}/sandboxes`, { signal });
      if (signal.aborted || sequence !== loadSequence.current) return;
      setResult(next);
      setLoadError("");
    } catch (cause) {
      if (!signal.aborted && sequence === loadSequence.current) setLoadError(cause instanceof Error ? cause.message : "Couldn’t load sandboxes.");
    }
  }, [workspace.id, requestSignal]);
  useEffect(() => {
    queueMicrotask(() => void load());
    const interval = window.setInterval(() => void load(), 5000);
    return () => window.clearInterval(interval);
  }, [load]);
  const sandboxes = result?.sandboxes ?? [];
  return <AppShell active="Sandboxes" pageTitle="Sandboxes"><section className={styles.contentPage}>
    <div className={styles.pageHeading} data-motion="reveal"><div><h1>Sandboxes</h1><p>Cloud computers for you and your agents.{result ? ` ${freeHoursText(result.freeUsage.usedSeconds, result.freeUsage.limitSeconds)}` : ""}</p></div>
      {result?.enabled ? <div className={styles.pageActions}><Link className={styles.secondaryButton} href="/app/sandboxes/library">Saved environments</Link>{writer ? <><Link className={styles.secondaryButton} href={`/app?compose=1&prompt=${encodeURIComponent("Create a sandbox called ")}`}><WorkspaceIcon name="agent" width="16" height="16" />Ask Canter</Link><button className={styles.primaryButton} onClick={openDialog} disabled={!sizesReady}><WorkspaceIcon name="plus" width="16" height="16" />New sandbox</button></> : null}</div> : null}</div>
    {loadError ? <div role="alert" className={styles.error}>{loadError} <button onClick={() => void load()}>Try again</button></div> : !result ? <p role="status" className={styles.loading}>Loading sandboxes…</p> : !result.enabled ? <p>Enable Sandboxes in <Link href="/app/products">Products</Link> to get started.</p> : sandboxes.length ? <div className={styles.cardGrid}>{sandboxes.map(sandbox => <Link href={`/app/sandboxes/${encodeURIComponent(sandbox.id)}`} key={sandbox.id} className={styles.resourceCard} data-motion="fade" data-motion-card>
      <div className={styles.resourceCardHeader}><div><WorkspaceIcon name="terminal" /><h2>{sandbox.name}</h2></div><span className={styles.status} data-phase={sandbox.phase === "ready" ? "succeeded" : sandbox.phase === "save_failed" ? "failed" : sandbox.phase}>{sandboxPhaseText(sandbox.phase)}</span></div>
      <p>{sandbox.size} · {sandbox.tier === "paid" ? `${sandboxHourlyCents(sandbox.monthlyCents)}` : "free tier"}{sandbox.desktop ? " · Desktop" : ""} · by {sandbox.createdByName || (sandbox.createdByKind === "agent" ? "an agent" : "a member")}</p>
      <div className={styles.resourceCardFooter}><span>{sandbox.lastUsedAt ? `Active ${relativeTime(sandbox.lastUsedAt)}` : "Not used yet"}</span><span>{sandbox.createdAt ? `Created ${relativeTime(sandbox.createdAt)}` : ""}</span></div>
      {sandbox.labels && Object.keys(sandbox.labels).length ? <p className={styles.muted}>{Object.entries(sandbox.labels).map(([k, v]) => `${k}=${v}`).join(" ")}</p> : null}
    </Link>)}</div> : <div className={styles.emptyState} data-motion="reveal"><span className={styles.emptyIcon}><WorkspaceIcon name="terminal" width="24" height="24" /></span><h2>No sandboxes yet</h2><p>Sandboxes are private cloud computers that run code, keep files, and sleep when idle.</p></div>}
    {dialog && writer ? <CreateDialog draft={draft} setDraft={setDraft} sizes={result?.sizes ?? []} defaultIdle={result?.limits?.defaultIdleSeconds ?? 900} error={createError} pending={creating} locked={submitted !== null} onNewRequest={() => {
      setDraft(current => ({ ...current, key: crypto.randomUUID() }));
      setSubmitted(null);
      setCreateError("");
    }} onClose={() => setDialog(false)} onSubmit={async (event: FormEvent) => {
      event.preventDefault();
      const signal = requestSignal();
      if (creating || signal.aborted || !writer) return;
      setCreateError("");
      try {
        const paid = !(result?.sizes ?? []).find(s => s.id === draft.size)?.free;
        const body = submitted ?? JSON.stringify({ name: draft.name, size: draft.size, labels: parseSandboxLabels(draft.labels), idleTimeoutSeconds: paid ? Number(draft.idle) : undefined, desktop: draft.desktop, idempotencyKey: draft.key });
        setSubmitted(body);
        setCreating(true);
        await canterFetch(`/workspaces/${encodeURIComponent(workspace.id)}/sandboxes`, {
          method: "POST", body, signal,
        });
        if (signal.aborted) return;
        setDialog(false);
        void load();
      } catch (cause) {
        if (!signal.aborted) setCreateError(cause instanceof Error ? cause.message : "Couldn’t create the sandbox.");
      } finally { if (!signal.aborted) setCreating(false); }
    }} /> : null}
  </section></AppShell>;
}

function CreateDialog({ draft, setDraft, sizes, defaultIdle, error, pending, locked, onNewRequest, onClose, onSubmit }: { draft: { name: string; size: string; idle: string; labels: string; key: string; desktop: boolean }; setDraft: (d: { name: string; size: string; idle: string; labels: string; key: string; desktop: boolean }) => void; sizes: { id: string; vcpus: number; memoryMiB: number; diskGiB: number; free: boolean; monthlyCents: number }[]; defaultIdle: number; error: string; pending: boolean; locked: boolean; onNewRequest: () => void; onClose: () => void; onSubmit: (e: FormEvent) => void }) {
  const paid = !sizes.find(s => s.id === draft.size)?.free;
  const ref = useRef<HTMLDialogElement>(null);
  const dismiss = useDialog(ref, "input");
  function close() { if (!pending) { dismiss(onClose); } }
  return <dialog ref={ref} className={dialogStyles.dialog} aria-labelledby="create-sandbox-title" onCancel={event => { event.preventDefault(); close(); }}>
    <button className={dialogStyles.close} aria-label="Close" disabled={pending} onClick={close}><WorkspaceIcon name="close" width="17" height="17" /></button>
    <h2 id="create-sandbox-title">New sandbox</h2>
    <form onSubmit={onSubmit}>
      <label className={dialogStyles.field}>Name<input type="text" value={draft.name} onChange={e => setDraft({ ...draft, name: e.target.value })} maxLength={80} required disabled={pending || locked} /></label>
      <label className={dialogStyles.field}>Size<select value={draft.size} onChange={e => {
        const next = e.target.value;
        setDraft({ ...draft, size: next, idle: "-1", ...(sandboxDesktopEligible(sizes.find(s => s.id === next)) ? {} : { desktop: false }) });
      }} disabled={pending || locked}>{sizes.map(size => <option key={size.id} value={size.id}>{size.id} — {size.vcpus} vCPU, {formatMiB(size.memoryMiB)} RAM, {size.diskGiB} GiB disk{size.free ? " (free tier)" : ` (${sandboxHourlyCents(size.monthlyCents)})`}</option>)}</select></label>
      {paid ? <label className={dialogStyles.field}>Idle sleep<select value={draft.idle} onChange={e => setDraft({ ...draft, idle: e.target.value })} disabled={pending || locked}>
        <option value="-1">{idleChoiceText(-1, defaultIdle)}</option>
        <option value="0">Never sleep while running</option>
        {[300, 900, 1800, 3600, 7200].map(v => <option key={v} value={v}>{idleChoiceText(v, defaultIdle)}</option>)}
        {!["-1", "0", "300", "900", "1800", "3600", "7200"].includes(draft.idle) ? <option value={draft.idle}>{idleChoiceText(Number(draft.idle), defaultIdle)}</option> : null}
      </select></label> : <p>Free sandboxes sleep after 15 idle minutes and share 50 awake-hours monthly.</p>}
      <label className={dialogStyles.field} style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
        <input type="checkbox" checked={draft.desktop} onChange={e => setDraft({ ...draft, desktop: e.target.checked })} disabled={pending || locked || !sandboxDesktopEligible(sizes.find(s => s.id === draft.size))} />
        <span>Desktop</span>
      </label>
      <p className={dialogStyles.field} style={{ marginTop: -8 }}>A shared graphical desktop. Requires at least 2 vCPUs and 4 GiB RAM.</p>
      <label className={dialogStyles.field}>Labels <span>(optional JSON object — values may contain spaces)</span><textarea value={draft.labels} onChange={e => setDraft({ ...draft, labels: e.target.value })} placeholder={'{"team": "platform", "env": "dev"}'} disabled={pending || locked} rows={2} style={{ fontFamily: "monospace", fontSize: 12 }} /></label>
      {error ? <p className={dialogStyles.error} role="alert">{error}</p> : null}
      {locked && !pending ? <p>Retry sends the same request. Check your sandbox list before starting a separate request. <button type="button" className={styles.secondaryButton} onClick={onNewRequest}>Edit as new request</button></p> : null}
      <button className={dialogStyles.primary} type="submit" disabled={pending}>{pending ? "Creating…" : locked ? "Retry request" : "Create sandbox"}</button>
    </form>
  </dialog>;
}
