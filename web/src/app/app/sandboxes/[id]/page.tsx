"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { AppShell } from "@/components/app-shell";
import { useWorkspace } from "@/components/workspace-context";
import { useRequestScope } from "@/components/use-request-scope";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { canterFetch, relativeTime } from "@/lib/canter-api";
import { sandboxPhaseText,  formatBytes, type Sandbox, type SandboxMetrics, type SandboxListResponse } from "@/lib/sandboxes";
import SandboxSetupProgress from "@/components/sandbox-setup-progress";
import { followJob, parseSandboxLabels, runUpload, sandboxDownloadURL, type FollowState, type UploadLimits } from "@/lib/sandbox-client";
import { SandboxSavedEnvironments } from "@/components/sandbox-saved-environments";
import { SandboxDesktop } from "@/components/sandbox-desktop";
import { SandboxTerminal } from "@/components/sandbox-terminal";
import styles from "@/components/workspace.module.css";

type JobState = { jobId: string; command: string; status: string; exitCode?: number; text: string; offset: number; truncated?: boolean; error?: string };
type FileEntry = { name: string; directory: boolean; size: number };
type Preview = { id: string; port: number; url: string; createdBy: string; createdAt: string };

export default function SandboxDetailPage() {
  const { id } = useParams<{ id: string }>();
  const { data, error } = useWorkspace();
  if (!data) return <AppShell active="Sandboxes"><div className={styles.contentPage}><p role={error ? "alert" : "status"}>{error ? "Couldn’t load this organization." : "Loading…"}</p></div></AppShell>;
  // Keying on workspace:id fully resets all sandbox state — in-flight effects,
  // uploads, job follows and form drafts can never repaint into another org.
  return <SandboxDetail key={`${data.workspace.id}:${id}`} workspace={data} id={id} />;
}

function SandboxDetail({ workspace, id }: { workspace: ReturnType<typeof useWorkspace>["data"] & object; id: string }) {
  const { signal: requestSignal } = useRequestScope();
  const { signal: machineSignal, reset: resetMachineRequests } = useRequestScope();
  const machineGeneration = useRef<number | null>(null);
  const router = useRouter();
  const writer = workspace.workspace.role === "owner" || workspace.workspace.role === "operator";
  const data = workspace;
  const [sandbox, setSandbox] = useState<Sandbox | null>(null);
  const setupPending = sandbox?.setup?.status === "pending" || sandbox?.setup?.status === "running";
  const canMutate = writer && sandbox?.phase === "ready" && !setupPending;
  const [loadError, setLoadError] = useState("");
  const [actionError, setActionError] = useState("");
  const [pending, setPending] = useState("");
  const [command, setCommand] = useState("");
  const [running, setRunning] = useState(false);
  const [path, setPath] = useState("/workspace");
  const [entries, setEntries] = useState<FileEntry[] | null>(null);
  const [fileContent, setFileContent] = useState<{ path: string; content: string; truncated: boolean } | null>(null);
  const [tab, setTab] = useState<"exec" | "terminal" | "desktop">("exec");
  const [useSavedSecrets, setUseSavedSecrets] = useState(true);
  const [showSaved, setShowSaved] = useState(false);
  const [previews, setPreviews] = useState<Preview[] | null>(null);
  const [previewsAvailable, setPreviewsAvailable] = useState(false);
  const [previewPort, setPreviewPort] = useState("");
  const [directory, setDirectory] = useState("/workspace");
  const [timeoutSec, setTimeoutSec] = useState("");
  const [job, setJob] = useState<JobState | null>(null);
  const [stopping, setStopping] = useState(false);
  const [metrics, setMetrics] = useState<SandboxMetrics | null>(null);
  const [metricsError, setMetricsError] = useState("");
  const [upload, setUpload] = useState<{ name: string; done: number; size: number; error?: string } | null>(null);
  const [limits, setLimits] = useState<UploadLimits | null>(null);
  const [limitsError, setLimitsError] = useState("");
  const loadSequence = useRef(0);
  const browseSequence = useRef(0);
  const currentDirectory = useRef("/workspace");
  const uploadAbort = useRef<AbortController | null>(null);
  const jobAbort = useRef<AbortController | null>(null);
  const base = data ? `/workspaces/${encodeURIComponent(data.workspace.id)}/sandboxes/${encodeURIComponent(id)}` : "";
  const acceptSandbox = useCallback((value: Sandbox) => {
    if (machineGeneration.current !== null && machineGeneration.current !== (value.generation ?? 0)) {
      resetMachineRequests();
      jobAbort.current?.abort(); uploadAbort.current?.abort();
      setJob(null); setUpload(null); setRunning(false); setStopping(false);
      setEntries(null); setFileContent(null); setMetrics(null); setMetricsError("");
      setPreviews(null); setPreviewsAvailable(false);
    }
    machineGeneration.current = value.generation ?? 0;
    setSandbox(value);
  }, [resetMachineRequests]);
  const load = useCallback(async () => {
    const signal = requestSignal();
    if (!base || signal.aborted) return;
    const sequence = ++loadSequence.current;
    try {
      const value = await canterFetch<Sandbox>(base, { signal });
      if (signal.aborted || sequence !== loadSequence.current) return;
      acceptSandbox(value); setLoadError("");
    } catch (cause) {
      if (!signal.aborted && sequence === loadSequence.current) setLoadError(cause instanceof Error ? cause.message : "Couldn’t load this sandbox.");
    }
  }, [base, requestSignal, acceptSandbox]);
  const loadLimits = useCallback(async () => {
    const signal = requestSignal();
    if (signal.aborted) return;
    try {
      const list = await canterFetch<SandboxListResponse>(base.replace(/\/sandboxes\/[^/]+$/, "/sandboxes"), { signal });
      if (signal.aborted) return;
      if (!list.limits || !Number.isSafeInteger(list.limits.maxFileBytes) || list.limits.maxFileBytes < 0 ||
          !Number.isSafeInteger(list.limits.uploadChunkBytes) || list.limits.uploadChunkBytes < 1) throw new Error("File uploads are not available on this server.");
      setLimits(list.limits); setLimitsError("");
    } catch (cause) {
      if (!signal.aborted) { setLimits(null); setLimitsError(cause instanceof Error ? cause.message : "Couldn’t load upload limits."); }
    }
  }, [base, requestSignal]);
  useEffect(() => { if (writer) queueMicrotask(() => void loadLimits()); }, [writer, loadLimits]);
  const browse = useCallback(async (target: string) => {
    const signal = machineSignal();
    if (!base || signal.aborted) return;
    const sequence = ++browseSequence.current;
    try {
      const result = await canterFetch<{ path: string; entries?: FileEntry[]; content?: string; eof?: boolean }>(`${base}/files?path=${encodeURIComponent(target)}`, { signal });
      if (signal.aborted || sequence !== browseSequence.current) return;
      setActionError("");
      if (result.entries) { setEntries(result.entries); setPath(result.path); currentDirectory.current = result.path; setFileContent(null); }
      else setFileContent({ path: result.path, content: result.content ?? "", truncated: result.eof === false });
    } catch (cause) { if (!signal.aborted && sequence === browseSequence.current) setActionError(cause instanceof Error ? cause.message : "Couldn't read that path."); }
  }, [base, machineSignal]);
  useEffect(() => {
    queueMicrotask(() => void load());
    const interval = window.setInterval(() => void load(), 5000);
    return () => window.clearInterval(interval);
  }, [load]);
  useEffect(() => { if (sandbox?.phase === "ready" || sandbox?.phase === "save_failed") queueMicrotask(() => void browse(currentDirectory.current)); }, [sandbox?.phase, sandbox?.generation, browse]);
  const loadPreviews = useCallback(async () => {
    const signal = machineSignal();
    if (!base || signal.aborted) return;
    try {
      const result = await canterFetch<{ previews: Preview[] }>(`${base}/previews`, { signal });
      if (signal.aborted) return;
      setPreviews(result.previews); setPreviewsAvailable(true);
    } catch { if (!signal.aborted) setPreviewsAvailable(false); }
  }, [base, machineSignal]);
  useEffect(() => { if (sandbox?.phase === "ready") queueMicrotask(() => void loadPreviews()); }, [sandbox?.phase, sandbox?.generation, loadPreviews]);
  const loadMetrics = useCallback(async () => {
    const signal = machineSignal();
    if (!base || sandbox?.phase !== "ready" || signal.aborted) return;
    try {
      const value = await canterFetch<SandboxMetrics>(`${base}/metrics`, { signal });
      if (signal.aborted) return;
      setMetrics(value);
      setMetricsError("");
    } catch (cause) { if (!signal.aborted) setMetricsError(cause instanceof Error ? cause.message : "Metrics unavailable."); }
  }, [base, sandbox?.phase, machineSignal]);
  useEffect(() => {
    if (sandbox?.phase !== "ready") return;
    let live = true;
    const tick = async () => { if (live && !document.hidden) await loadMetrics(); };
    queueMicrotask(() => void tick());
    const interval = window.setInterval(() => void tick(), 10000);
    return () => { live = false; window.clearInterval(interval); };
  }, [sandbox?.phase, sandbox?.generation, loadMetrics]);
  async function sharePreview(event: FormEvent) {
    event.preventDefault();
    const port = Number(previewPort);
    const signal = requestSignal();
    if (!base || !port || signal.aborted || !canMutate) return;
    setPending("preview");
    setActionError("");
    try {
      const created = await canterFetch<Preview>(`${base}/previews`, { method: "POST", body: JSON.stringify({ port }), signal });
      if (signal.aborted) return;
      setPreviews(list => [...(list ?? []), created]);
      setPreviewPort("");
    } catch (cause) { if (!signal.aborted) setActionError(cause instanceof Error ? cause.message : "Could not create the preview."); }
    finally { if (!signal.aborted) setPending(""); }
  }
  async function revokePreview(pid: string) {
    const signal = requestSignal();
    if (signal.aborted || !writer) return;
    try {
      await canterFetch(`${base}/previews/${encodeURIComponent(pid)}`, { method: "DELETE", signal });
      if (!signal.aborted) setPreviews(list => (list ?? []).filter(item => item.id !== pid));
    } catch (cause) { if (!signal.aborted) setActionError(cause instanceof Error ? cause.message : "Could not revoke the preview."); }
  }
  async function action(kind: string) {
    const signal = requestSignal();
    if (!base || signal.aborted || !writer) return;
    setPending(kind);
    setActionError("");
    try {
      if (kind === "delete") await canterFetch(base, { method: "DELETE", signal });
      else await canterFetch(`${base}/${kind}`, { method: "POST", signal });
      if (signal.aborted) return;
      if (kind === "delete") { router.replace("/app/sandboxes"); return; }
      await load();
    } catch (cause) { if (!signal.aborted) setActionError(cause instanceof Error ? cause.message : "That action didn't apply."); }
    finally { if (!signal.aborted) setPending(""); }
  }
  const stopJob = useCallback(async () => {
    const signal = machineSignal();
    if (!base || !job || signal.aborted || !writer) return;
    const jobId = job.jobId;
    setStopping(true);
    try {
      const view = await canterFetch<{ status: string; exitCode?: number }>(`${base}/jobs/${encodeURIComponent(jobId)}/stop`, { method: "POST", signal });
      if (!signal.aborted) setJob(current => current?.jobId === jobId ? { ...current, status: view.status, exitCode: view.exitCode } : current);
    } catch (cause) { if (!signal.aborted) setActionError(cause instanceof Error ? cause.message : "Couldn’t stop the job."); }
    finally { if (!signal.aborted) setStopping(false); }
  }, [base, job, machineSignal, writer]);

  const follow = useCallback((jobId: string) => {
    jobAbort.current?.abort();
    const abort = new AbortController();
    jobAbort.current = abort;
    const signal = machineSignal();
    if (signal.aborted) return;
    const cancel = () => abort.abort();
    signal.addEventListener("abort", cancel, { once: true });
    abort.signal.addEventListener("abort", () => signal.removeEventListener("abort", cancel), { once: true });
    const stop = followJob(base, jobId, (state: FollowState) => {
      if (signal.aborted) { stop(); return; }
      setJob(current => current?.jobId === jobId ? { ...current, text: state.text, offset: state.offset, status: state.status, exitCode: state.exitCode, truncated: state.truncated, error: state.error } : current);
    }, abort.signal);
    return stop;
  }, [base, machineSignal]);

  // Keyed remount clears all state; unmount aborts every in-flight operation.
  useEffect(() => () => { jobAbort.current?.abort(); uploadAbort.current?.abort(); }, []);

  async function exec(event: FormEvent) {
    event.preventDefault();
    const signal = machineSignal();
    if (!base || !command.trim() || signal.aborted || !canMutate || running) return;
    const submittedCommand = command.trim();
    setRunning(true);
    setActionError("");
    jobAbort.current?.abort();
    setJob(null);
    try {
      const body: Record<string, unknown> = { command: submittedCommand };
      if (directory.trim() && directory.trim() !== "/workspace") body.directory = directory.trim();
      const seconds = Number(timeoutSec);
      if (timeoutSec && (!Number.isInteger(seconds) || seconds < 1 || seconds > 86400)) throw new Error("Choose a timeout between 1 and 86400 seconds.");
      if (timeoutSec) body.timeoutSeconds = seconds;
      if (savedSecretNames.length > 0 && !useSavedSecrets) body.secretRefs = {};
      const started = await canterFetch<{ jobId: string; status: string }>(`${base}/jobs`, { method: "POST", body: JSON.stringify(body), signal });
      if (signal.aborted) return;
      setJob({ jobId: started.jobId, command: submittedCommand, status: started.status, text: "", offset: 0 });
      follow(started.jobId);
    } catch (cause) { if (!signal.aborted) setActionError(cause instanceof Error ? cause.message : "The command didn't run."); }
    finally { if (!signal.aborted) setRunning(false); }
  }

  const uploadFileRef = useRef<{ file: File; target: string } | null>(null);
  async function uploadFile(event: React.ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file) return;
    const selection = { file, target: currentDirectory.current + "/" + file.name };
    uploadFileRef.current = selection;
    await startUpload(selection);
  }
  async function startUpload({ file, target }: { file: File; target: string }) {
    const signal = machineSignal();
    if (!base || signal.aborted || !canMutate || !limits) return;
    uploadAbort.current?.abort();
    const abort = new AbortController();
    const cancel = () => abort.abort();
    signal.addEventListener("abort", cancel, { once: true });
    uploadAbort.current = abort;
    setUpload({ name: file.name, done: 0, size: file.size });
    setActionError("");
    try {
      await runUpload(base, target, file, limits,
        p => { if (!signal.aborted && uploadAbort.current === abort) setUpload({ name: file.name, done: p.done, size: p.size }); }, abort.signal);
      if (signal.aborted || abort.signal.aborted || uploadAbort.current !== abort) return;
      setUpload(null);
      uploadFileRef.current = null;
      void browse(currentDirectory.current);
    } catch (cause) {
      if (!signal.aborted && !abort.signal.aborted && uploadAbort.current === abort) setUpload(current => current ? { ...current, error: cause instanceof Error ? cause.message : "Upload failed." } : current);
    } finally {
      signal.removeEventListener("abort", cancel);
    }
  }
  function cancelUpload() {
    uploadAbort.current?.abort();
    setUpload(null);
    uploadFileRef.current = null;
  }
  async function retryUpload() {
    if (uploadFileRef.current) await startUpload(uploadFileRef.current);
  }
  const ready = sandbox?.phase === "ready";
  const saveFailed = sandbox?.phase === "save_failed";
  const readable = ready || saveFailed;
  const setupStatus = sandbox?.setup?.status ?? "";
  const savedSecretNames = sandbox?.environment?.secretRefs ? Object.keys(sandbox.environment.secretRefs) : [];
  return <AppShell active="Sandboxes" pageTitle={sandbox?.name ?? "Sandbox"} pageKey={base}><section className={styles.contentPage}>
    <nav className={styles.breadcrumb ?? ""}><Link href="/app/sandboxes" className={styles.secondaryButton}><WorkspaceIcon name="arrow" width="14" height="14" />Sandboxes</Link></nav>
    {loadError && !sandbox ? <div role="alert" className={styles.error}>{loadError} <button onClick={() => void load()}>Try again</button></div> : !sandbox ? <p role="status" className={styles.loading}>Loading…</p> : <>
      <div className={styles.pageHeading} data-motion="reveal"><div><h1>{sandbox.name}</h1><p>{sandbox.size} · {sandbox.tier} tier{sandbox.createdByName ? ` · created by ${sandbox.createdByName}` : ""}{sandbox.lastUsedAt ? ` · active ${relativeTime(sandbox.lastUsedAt)}` : ""}</p></div>
        <div className={styles.pageActions}><span className={styles.status} data-phase={ready && setupStatus === "failed" ? "failed" : ready && setupPending ? "running" : ready ? "succeeded" : saveFailed ? "failed" : sandbox.phase}>{ready && setupPending ? `Setup ${setupStatus}` : ready && setupStatus === "failed" ? "Setup failed" : sandboxPhaseText(sandbox.phase)}</span>
          {writer && ready ? <button className={styles.secondaryButton} disabled={pending !== ""} onClick={() => void action("sleep")}>Sleep</button> : null}
          {writer && sandbox.phase === "asleep" ? <button className={styles.primaryButton} disabled={pending !== ""} onClick={() => void action("wake")}>Wake</button> : null}
          {writer && saveFailed ? <>
            <button className={styles.secondaryButton} disabled={pending !== ""} onClick={() => void action("sleep")}>Retry sleep</button>
            <button className={styles.primaryButton} disabled={pending !== ""} onClick={() => void action("wake")}>Resume</button>
          </> : null}
          {writer ? <button className={styles.secondaryButton} disabled={pending !== ""} onClick={() => { if (window.confirm(`Delete ${sandbox.name}? Unkept saves not used by other sandboxes or templates will be removed permanently.`)) void action("delete"); }}>Delete</button> : null}
        </div></div>
      {sandbox.failure ? <p role="alert" className={styles.error}>{sandbox.failure}</p> : null}
      {sandbox.labels && Object.keys(sandbox.labels).length ? <p className={styles.status}>{Object.entries(sandbox.labels).map(([key, value]) => `${key}=${value}`).join(" · ")}</p> : null}
      {actionError ? <p role="alert" className={styles.error}>{actionError}</p> : null}
      {!writer ? <p>You have read-only access to this organization.</p> : null}
      {sandbox.phase === "asleep" ? <p className={styles.loading}>This sandbox is asleep; its saved files restore on wake.</p> : null}
      {setupPending ? <p className={styles.status} role="status">New commands, uploads, terminals, desktop control and previews wait for setup to finish.</p> : null}
      {sandbox.setup?.status ? <SandboxSetupProgress key={`${sandbox.id}:${sandbox.generation}:${sandbox.setup.jobId ?? "none"}`} base={base} setup={sandbox.setup} writer={writer} available={ready} onRefresh={() => void load()} /> : null}
      {ready ? <>
        <div style={{ display: "flex", gap: 8, marginBottom: 16 }}>
          <button className={tab === "exec" ? styles.primaryButton : styles.secondaryButton} onClick={() => setTab("exec")}>Run command</button>
          <button className={tab === "terminal" ? styles.primaryButton : styles.secondaryButton} disabled={!canMutate} onClick={() => setTab("terminal")}>Terminal</button>
          {sandbox?.desktop && sandbox.phase === "ready" ? <button className={tab === "desktop" ? styles.primaryButton : styles.secondaryButton} onClick={() => setTab("desktop")}>Desktop</button> : null}
        </div>
        {tab === "desktop" && sandbox ? <SandboxDesktop key={`${sandbox.id}:${sandbox.generation}`} base={`/workspaces/${encodeURIComponent(data.workspace.id)}/sandboxes/${encodeURIComponent(sandbox.id)}`} sandbox={sandbox} accountId={data.account.id} writer={canMutate} /> : tab === "terminal" ? <section><h2>Terminal</h2>
          {canMutate ? <SandboxTerminal base={base} interactive={canMutate} /> : <p className={styles.status}>Terminals require write access and completed setup.</p>}
        </section> : <>
        <section><h2>Run command</h2>
        {writer && savedSecretNames.length > 0 ? <label style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 8 }}><input type="checkbox" checked={useSavedSecrets} onChange={event => setUseSavedSecrets(event.target.checked)} disabled={running || !canMutate} />Use saved secrets ({savedSecretNames.join(", ")})</label> : null}
        <form onSubmit={exec}>
          <div style={{ display: "flex", gap: 8 }}>
            <input aria-label="Command" placeholder="ls -la /workspace" value={command} onChange={event => setCommand(event.target.value)} disabled={!canMutate || running} style={{ flex: 1, background: "#1c1c1c", border: "1px solid #363636", borderRadius: 7, color: "#e4e4e4", padding: "9px 12px", fontFamily: "monospace", fontSize: 13 }} />
            <button className={styles.primaryButton} type="submit" disabled={!canMutate || running || !command.trim()}>{running ? "Starting…" : "Run"}</button>
          </div>
          {writer ? <div style={{ display: "flex", gap: 8, marginTop: 8 }}>
            <input aria-label="Directory" placeholder="/workspace" value={directory} onChange={event => setDirectory(event.target.value)} style={{ flex: 2, background: "#1c1c1c", border: "1px solid #363636", borderRadius: 7, color: "#e4e4e4", padding: "7px 10px", fontFamily: "monospace", fontSize: 12 }} />
            <input aria-label="Timeout (seconds)" placeholder="3600" inputMode="numeric" value={timeoutSec} onChange={event => setTimeoutSec(event.target.value.replace(/\D/g, ""))} style={{ flex: 1, background: "#1c1c1c", border: "1px solid #363636", borderRadius: 7, color: "#e4e4e4", padding: "7px 10px", fontFamily: "monospace", fontSize: 12 }} />
          </div> : null}
        </form>
        {job ? <div style={{ marginTop: 12 }}>
          <div style={{ display: "flex", gap: 8, alignItems: "center", marginBottom: 6 }}>
            <span className={styles.status} data-phase={job.status === "completed" ? "succeeded" : job.status}>{job.status}{job.exitCode != null ? ` · exit ${job.exitCode}` : ""}</span>
            {(job.status === "starting" || job.status === "running") && writer ? <button className={styles.secondaryButton} disabled={stopping} onClick={() => void stopJob()}>{stopping ? "Stopping…" : "Stop"}</button> : null}
            {job.error ? <><span role="alert" className={styles.error}>{job.error}</span><button className={styles.secondaryButton} onClick={() => follow(job.jobId)}>Reconnect logs</button></> : null}
            {job.truncated ? <span className={styles.status}>log truncated</span> : null}
          </div>
          <pre style={{ padding: 12, background: "#141414", border: "1px solid #2c2c2c", borderRadius: 8, fontSize: 12, overflowX: "auto", whiteSpace: "pre-wrap" }}>{`$ ${job.command}\n${job.text || (["starting", "running"].includes(job.status) ? "(waiting for output…)" : "(no output)")}`}</pre>
        </div> : null}</section>
      </>}
      </> : null}
      {readable ? <>
        <section style={{ marginTop: 28 }}><h2>Files <span className={styles.status}>{fileContent?.path ?? path}</span></h2>
          {writer && ready ? <div style={{ display: "flex", gap: 8, marginBottom: 12, alignItems: "center" }}>
            <input type="file" id="sandbox-upload" onChange={uploadFile} style={{ fontSize: 13 }} disabled={!canMutate || !!upload || !limits} />
            {upload ? <span className={styles.status}>{upload.name}: {formatBytes(upload.done)} / {formatBytes(upload.size)} {upload.error ? <><em className={styles.error} role="alert">{upload.error}</em> <button className={styles.secondaryButton} style={{ fontSize: 11 }} onClick={() => void retryUpload()}>Retry</button></> : null}<button className={styles.secondaryButton} onClick={cancelUpload} style={{ marginLeft: 8 }}>Cancel</button></span> : null}
          </div> : null}
          {writer && ready && limitsError ? <p role="alert" className={styles.error}>{limitsError} <button className={styles.secondaryButton} onClick={() => void loadLimits()}>Retry</button></p> : null}
          {!fileContent && path !== "/workspace" ? <button className={styles.secondaryButton} onClick={() => void browse(path.slice(0, path.lastIndexOf("/")) || "/workspace")}>Up</button> : null}
          {fileContent ? <div style={{ display: "flex", gap: 8, marginBottom: 8 }}>
            <button className={styles.secondaryButton} onClick={() => setFileContent(null)}>Back</button>
            <a className={styles.secondaryButton} href={sandboxDownloadURL(base, fileContent.path)} download>Download</a>
          </div> : null}
          {fileContent?.truncated ? <p className={styles.status}>Showing the beginning of this file. Download for the full contents.</p> : null}
          {fileContent ? <pre style={{ padding: 12, background: "#141414", border: "1px solid #2c2c2c", borderRadius: 8, fontSize: 12, overflowX: "auto", whiteSpace: "pre-wrap" }}>{fileContent.content}</pre> : <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>{(entries ?? []).map(entry => <li key={entry.name} style={{ padding: "6px 0", display: "flex", gap: 8, alignItems: "center" }}><button className={styles.secondaryButton} onClick={() => void browse(path === "/" ? `/${entry.name}` : `${path}/${entry.name}`)}><WorkspaceIcon name={entry.directory ? "folder" : "file"} width="14" height="14" />{entry.name}</button>{!entry.directory ? <a className={styles.secondaryButton} href={sandboxDownloadURL(base, (path === "/" ? "/" : path + "/") + entry.name)} download style={{ fontSize: 12 }}>Download</a> : null}</li>)}{entries === null ? <li className={styles.status}>Loading…</li> : entries.length === 0 ? <li className={styles.status}>Empty directory</li> : null}</ul>}</section>
      </> : null}
      {ready ? <>
        {metrics || metricsError ? <section style={{ marginTop: 28 }}><h2>Resources</h2>
          {metricsError && metrics ? <p className={styles.status} role="status">Metrics are stale — last successful read shown.</p> : null}
          {metrics ? <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(140px, 1fr))", gap: 12 }}>
            <div><p className={styles.status}>Memory</p><p style={{ fontSize: 18, fontWeight: 600 }}>{formatBytes(metrics.memory.usedBytes)} / {formatBytes(metrics.memory.totalBytes)}</p></div>
            <div><p className={styles.status}>Disk</p><p style={{ fontSize: 18, fontWeight: 600 }}>{formatBytes(metrics.disk.usedBytes)} / {formatBytes(metrics.disk.totalBytes)}</p></div>
            <div><p className={styles.status}>CPU load (1 min)</p><p style={{ fontSize: 18, fontWeight: 600 }}>{metrics.cpu.load1.toFixed(2)} <span className={styles.status}>· {metrics.cpu.count} vCPUs</span></p></div>
            <div><p className={styles.status}>Uptime</p><p style={{ fontSize: 18, fontWeight: 600 }}>{Math.round(metrics.uptimeSeconds / 60)} min</p></div>
          </div> : <p className={styles.status}>{metricsError || "Metrics unavailable."}</p>}
        </section> : null}
        {previewsAvailable ? <section style={{ marginTop: 28 }}><h2>Previews</h2>
          <p className={styles.status}>Anyone with the link can open this port.</p>
          {writer ? <form onSubmit={sharePreview} style={{ display: "flex", gap: 8, marginBottom: 12 }}>
            <input aria-label="Port" placeholder="3000" inputMode="numeric" value={previewPort} onChange={event => setPreviewPort(event.target.value.replace(/\D/g, ""))} style={{ width: 120, background: "#1c1c1c", border: "1px solid #363636", borderRadius: 7, color: "#e4e4e4", padding: "9px 12px", fontFamily: "monospace", fontSize: 13 }} />
            <button className={styles.primaryButton} type="submit" disabled={!canMutate || pending === "preview" || !previewPort}>Share preview</button>
          </form> : null}
          <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>{(previews ?? []).map(item => <li key={item.id} style={{ display: "flex", gap: 8, alignItems: "center", padding: "6px 0" }}>
            <code style={{ fontSize: 12 }}>:{item.port}</code>
            <a href={item.url} target="_blank" rel="noreferrer" className={styles.secondaryButton}>Open</a>
            <button className={styles.secondaryButton} onClick={() => void navigator.clipboard?.writeText(item.url)}>Copy link</button>
            {writer ? <button className={styles.secondaryButton} onClick={() => void revokePreview(item.id)}>Revoke</button> : null}
          </li>)}{previews?.length === 0 ? <li className={styles.status}>No active previews.</li> : null}</ul>
        </section> : null}
      </> : null}
      {sandbox.notice ? <p role="status" className={styles.loading}>{sandbox.notice}</p> : null}
      <section style={{ marginTop: 28 }}>
        <button className={styles.secondaryButton} onClick={() => setShowSaved(current => !current)}>{showSaved ? "Hide saved environments" : "Show saved environments"}</button>
      </section>
      {showSaved && sandbox ? <SandboxSavedEnvironments key={`${sandbox.id}:${sandbox.generation}:${sandbox.phase}:${sandbox.snapshotId ?? ''}`} workspaceId={data.workspace.id} writer={writer} sandbox={sandbox} onRestored={value => { ++loadSequence.current; acceptSandbox(value); }} /> : null}
      {saveFailed && !sandbox.failure ? <div role="alert" className={styles.error}>Saving files failed. Your VM and its files have been kept.</div> : null}
      {saveFailed && sandbox.tier === "paid" ? <p className={styles.loading}>Compute usage continues while this VM is running.</p> : null}
      {writer && !["deleting", "deleted"].includes(sandbox.phase) ? <SandboxSettings sandbox={sandbox} base={base} onSaved={value => { loadSequence.current++; acceptSandbox(value); }} /> : null}
    </>}
  </section></AppShell>;
}

function SandboxSettings({ sandbox, base, onSaved }: { sandbox: Sandbox; base: string; onSaved: (value: Sandbox) => void }) {
  const { signal: requestSignal } = useRequestScope();
  const paid = sandbox.tier === "paid";
  const [draft, setDraft] = useState<{ name?: string; labels?: string; idle?: string }>({});
  const name = draft.name ?? sandbox.name;
  const labels = draft.labels ?? (Object.keys(sandbox.labels ?? {}).length ? JSON.stringify(sandbox.labels) : "");
  const idle = draft.idle ?? String(sandbox.idleTimeoutSeconds ?? -1);
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const [saved, setSaved] = useState(false);
  async function save(event: FormEvent) {
    event.preventDefault();
    const signal = requestSignal();
    if (pending || signal.aborted) return;
    setPending(true);
    setError("");
    setSaved(false);
    try {
      const body: Record<string, unknown> = {};
      if (draft.name !== undefined) body.name = name.trim();
      if (draft.labels !== undefined) body.labels = parseSandboxLabels(labels);
      if (paid && draft.idle !== undefined) body.idleTimeoutSeconds = Number(idle);
      const updated = await canterFetch<Sandbox>(base, { method: "PATCH", body: JSON.stringify(body), signal });
      if (signal.aborted) return;
      onSaved(updated);
      setDraft({});
      setSaved(true);
    } catch (cause) { if (!signal.aborted) setError(cause instanceof Error ? cause.message : "Couldn't save settings."); }
    finally { if (!signal.aborted) setPending(false); }
  }
  const presetIdle = ["-1", "0", "300", "900", "1800", "3600", "7200"];
  return <section style={{ marginTop: 28 }}><h2>Settings</h2>
    <form onSubmit={save} style={{ display: "grid", gap: 12, maxWidth: 420 }}>
      <label className={styles.status}>Name<input value={name} onChange={e => { setDraft(current => ({ ...current, name: e.target.value })); setSaved(false); }} maxLength={80} required disabled={pending} style={{ display: "block", marginTop: 4, width: "100%", background: "#1c1c1c", border: "1px solid #363636", borderRadius: 7, color: "#e4e4e4", padding: "8px 11px", fontSize: 13 }} /></label>
      <label className={styles.status}>Labels <span>(optional JSON object of string values)</span><textarea value={labels} onChange={e => { setDraft(current => ({ ...current, labels: e.target.value })); setSaved(false); }} placeholder={'{"team": "platform"}'} disabled={pending} rows={2} style={{ display: "block", marginTop: 4, width: "100%", background: "#1c1c1c", border: "1px solid #363636", borderRadius: 7, color: "#e4e4e4", padding: "8px 11px", fontFamily: "monospace", fontSize: 12 }} /></label>
      {paid ? <label className={styles.status}>Idle sleep<select value={idle} onChange={e => { setDraft(current => ({ ...current, idle: e.target.value })); setSaved(false); }} disabled={pending} style={{ display: "block", marginTop: 4, width: "100%", background: "#1c1c1c", border: "1px solid #363636", borderRadius: 7, color: "#e4e4e4", padding: "8px 11px", fontSize: 13 }}>
        <option value="-1">Server default</option>
        <option value="0">Never sleep while running</option>
        {[300, 900, 1800, 3600, 7200].map(v => <option key={v} value={v}>{Math.round(v / 60)} minutes</option>)}
        {!presetIdle.includes(idle) ? <option value={idle}>{idle} seconds</option> : null}
      </select></label> : null}
      <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
        <button type="submit" className={styles.primaryButton} disabled={pending || Object.keys(draft).length === 0}>{pending ? "Saving…" : "Save settings"}</button>
        {saved ? <span className={styles.status}>Saved.</span> : null}
      </div>
      {error ? <p role="alert" className={styles.error}>{error}</p> : null}
    </form></section>;
}
