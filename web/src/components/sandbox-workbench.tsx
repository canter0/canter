"use client";

import Link from "next/link";
import { useCallback, useEffect, useRef, useState } from "react";
import { Input } from "@ui-sdk/react/input";
import { Menu, MenuContent, MenuItem, MenuTrigger } from "@ui-sdk/react/menu";
import { Tabs, TabsList, TabsPanel, TabsTab } from "@ui-sdk/react/tabs";
import { DashboardRequestDialog, type DashboardRequestSource } from "@/components/dashboard-request-action";
import { SandboxJobHistory } from "@/components/sandbox-job-history";
import { SandboxDesktop } from "@/components/sandbox-desktop";
import { SandboxSavedEnvironments } from "@/components/sandbox-saved-environments";
import { useRequestScope } from "@/components/use-request-scope";
import { useWorkspace } from "@/components/workspace-context";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { canterFetch, relativeTime } from "@/lib/canter-api";
import { sandboxRequestPrompt, type SandboxRequestAction } from "@/lib/dashboard-request-prompts";
import { formatBytes, sandboxHourlyCents, sandboxPhaseText, type Sandbox, type SandboxMetrics } from "@/lib/sandboxes";
import { followJob, sandboxDownloadURL, type FollowState } from "@/lib/sandbox-client";
import { workspaceDisplayName } from "@/lib/workspace-selection";
import { useWrenResource, workBase } from "@/lib/wren-work";
import styles from "@/components/workspace.module.css";
import consoleStyles from "@/components/sandbox-console.module.css";

type FileEntry = { name: string; directory: boolean; size: number };
type Preview = { id: string; port: number; url: string; createdBy: string; createdAt: string };
type JobState = { jobId: string; status: string; exitCode?: number; text: string; truncated?: boolean; error?: string };
type Section = "overview" | "files" | "jobs" | "saves" | "desktop";

// Keep polling stable states: mutations happen in conversations, and a stable
// list/detail must still reflect agent-side sleep/wake/create while visible.
const sandboxLive = (value: Sandbox) => value.phase !== "deleted";

export function SandboxWorkbench({ workspaceId, id, onRequest }: { workspaceId: string; id: string; onRequest?: (prompt: string) => void }) {
  const { data } = useWorkspace();
  const accountId = data?.account.id ?? "";
  // Keying on the full scope guarantees a coherent reset whenever account,
  // organization or machine changes — in a route or inside a chat surface.
  const scopedRole = data?.workspace.id === workspaceId ? data.workspace.role : undefined;
  return <SandboxWorkbenchInner key={`${accountId}:${workspaceId}:${id}`} accountId={accountId} role={scopedRole} workspaceName={data?.workspace.name} workspaceId={workspaceId} id={id} onRequest={onRequest} />;
}

function SandboxWorkbenchInner({ accountId, role, workspaceName, workspaceId, id, onRequest }: { accountId: string; role?: string; workspaceName?: string; workspaceId: string; id: string; onRequest?: (prompt: string) => void }) {
  const base = `${workBase(workspaceId)}/sandboxes/${encodeURIComponent(id)}`;
  const { signal: machineSignal, reset: resetMachineRequests } = useRequestScope();
  const resource = useWrenResource<Sandbox>(base, 5000, sandboxLive);
  const sandbox = resource.data ?? null;
  const machineGeneration = useRef<number | null>(null);

  const [section, setSection] = useState<Section>("overview");
  const [historyJob, setHistoryJob] = useState("");
  useEffect(() => {
    const restore = () => {
      const url = new URL(window.location.href);
      if (url.pathname !== `/app/sandboxes/${encodeURIComponent(id)}`) return;
      const job = url.searchParams.get("historyJob") ?? "";
      setHistoryJob(job);
      if (job) setSection("jobs");
    };
    window.queueMicrotask(restore);
    window.addEventListener("popstate", restore);
    return () => window.removeEventListener("popstate", restore);
  }, [id]);
  function selectHistoryJob(job: string) {
    setHistoryJob(job);
    const url = new URL(window.location.href);
    if (url.pathname !== `/app/sandboxes/${encodeURIComponent(id)}`) return;
    url.searchParams.set("historyJob", job);
    if (url.href !== window.location.href) window.history.pushState(null, "", url);
  }
  const [path, setPath] = useState("/workspace");
  const [entries, setEntries] = useState<FileEntry[] | null>(null);
  const [fileContent, setFileContent] = useState<{ path: string; content: string; truncated: boolean } | null>(null);
  const [filesError, setFilesError] = useState("");
  const [metrics, setMetrics] = useState<SandboxMetrics | null>(null);
  const [metricsError, setMetricsError] = useState("");
  const [previews, setPreviews] = useState<Preview[] | null>(null);
  const [previewsAvailable, setPreviewsAvailable] = useState(false);
  const [jobId, setJobId] = useState("");
  const [job, setJob] = useState<JobState | null>(null);
  const [jobError, setJobError] = useState("");
  const browseSequence = useRef(0);
  const currentDirectory = useRef("/workspace");
  const jobAbort = useRef<AbortController | null>(null);

  const [menuOpen, setMenuOpen] = useState(false);
  const pendingAction = useRef<SandboxRequestAction | null>(null);
  const moreTrigger = useRef<HTMLButtonElement>(null);
  const [dialog, setDialog] = useState<{ source: DashboardRequestSource; prompt: string } | null>(null);

  const ready = sandbox?.phase === "ready";
  const saveFailed = sandbox?.phase === "save_failed";
  const readable = ready || saveFailed;
  const asleep = sandbox?.phase === "asleep";
  const desktop = !!(sandbox?.desktop && ready);
  const writable = role === "owner" || role === "operator";

  const historyAvailable = !!sandbox && !["deleting", "deleted"].includes(sandbox.phase);
  const activeSection: Section = section === "jobs" && historyAvailable ? "jobs"
    : asleep && section !== "saves" && section !== "overview" ? "saves"
    : !readable && !asleep ? "overview"
    : section === "desktop" && !desktop ? "overview"
    : !readable && section === "files" ? "overview"
    : section;

  // A new generation (wake/recreate) or a ready→asleep/failed transition must
  // never repaint the previous machine's files, metrics, previews or followed
  // job; durable metadata and saved environments survive.
  const generation = sandbox?.generation ?? null;
  const readableSeen = useRef(false);
  useEffect(() => {
    const stale = generation !== null
      && ((machineGeneration.current !== null && machineGeneration.current !== generation)
        || (readableSeen.current && !readable));
    if (stale) {
      resetMachineRequests();
      jobAbort.current?.abort();
      setJob(null); setJobId(""); setJobError("");
      setEntries(null); setFileContent(null); setFilesError("");
      setMetrics(null); setMetricsError("");
      setPreviews(null); setPreviewsAvailable(false);
      currentDirectory.current = "/workspace";
      setPath("/workspace");
    }
    if (generation !== null) {
      machineGeneration.current = generation;
      readableSeen.current = readable;
    }
  }, [generation, readable, resetMachineRequests]);

  const browse = useCallback(async (target: string) => {
    const signal = machineSignal();
    if (signal.aborted) return;
    const sequence = ++browseSequence.current;
    try {
      const result = await canterFetch<{ path: string; entries?: FileEntry[]; content?: string; eof?: boolean }>(`${base}/files?path=${encodeURIComponent(target)}`, { signal });
      if (signal.aborted || sequence !== browseSequence.current) return;
      setFilesError("");
      if (result.entries) { setEntries(result.entries); setPath(result.path); currentDirectory.current = result.path; setFileContent(null); }
      else setFileContent({ path: result.path, content: result.content ?? "", truncated: result.eof === false });
    } catch (cause) { if (!signal.aborted && sequence === browseSequence.current) setFilesError(cause instanceof Error ? cause.message : "Couldn't read that path."); }
  }, [base, machineSignal]);

  useEffect(() => { if (readable && section === "files") queueMicrotask(() => void browse(currentDirectory.current)); }, [readable, section, generation, browse]);

  useEffect(() => {
    if (!ready) return;
    let live = true;
    const tick = async () => {
      const signal = machineSignal();
      if (!live || document.hidden || signal.aborted) return;
      try {
        const value = await canterFetch<SandboxMetrics>(`${base}/metrics`, { signal });
        if (signal.aborted || !live) return;
        setMetrics(value); setMetricsError("");
      } catch (cause) { if (!signal.aborted && live) setMetricsError(cause instanceof Error ? cause.message : "Metrics unavailable."); }
    };
    queueMicrotask(() => void tick());
    const interval = window.setInterval(() => void tick(), 10000);
    return () => { live = false; window.clearInterval(interval); };
  }, [ready, generation, base, machineSignal]);

  const loadPreviews = useCallback(async () => {
    const signal = machineSignal();
    if (signal.aborted) return;
    try {
      const result = await canterFetch<{ previews: Preview[] }>(`${base}/previews`, { signal });
      if (signal.aborted) return;
      setPreviews(result.previews); setPreviewsAvailable(true);
    } catch { if (!signal.aborted) setPreviewsAvailable(false); }
  }, [base, machineSignal]);
  useEffect(() => { if (ready) queueMicrotask(() => void loadPreviews()); }, [ready, generation, loadPreviews]);

  const follow = useCallback((target: string) => {
    jobAbort.current?.abort();
    const abort = new AbortController();
    jobAbort.current = abort;
    const signal = machineSignal();
    if (signal.aborted) return;
    const cancel = () => abort.abort();
    signal.addEventListener("abort", cancel, { once: true });
    abort.signal.addEventListener("abort", () => signal.removeEventListener("abort", cancel), { once: true });
    setJob({ jobId: target, status: "starting", text: "" });
    setJobError("");
    followJob(base, target, (state: FollowState) => {
      if (signal.aborted) { return; }
      setJob(current => current?.jobId === target ? { ...current, text: state.text, status: state.status, exitCode: state.exitCode, truncated: state.truncated, error: state.error } : current);
    }, abort.signal);
  }, [base, machineSignal]);
  useEffect(() => () => { jobAbort.current?.abort(); }, []);

  function inspectJob(event: React.FormEvent) {
    event.preventDefault();
    const target = jobId.trim();
    if (!target) return;
    follow(target);
  }

  function requestPrompt(action: SandboxRequestAction) {
    if (!sandbox) return "";
    return sandboxRequestPrompt(action, sandbox);
  }
  // Requests either append to the embedding chat or open the chooser with the
  // scope captured now — the dialog lives outside the menu so dismissal of
  // the popup never takes the request with it.
  function ask(action: SandboxRequestAction) {
    if (!writable) return;
    const prompt = requestPrompt(action);
    if (!prompt) return;
    if (onRequest) onRequest(prompt);
    else setDialog({ source: { accountId, workspaceId, workspaceName: workspaceDisplayName(workspaceName) }, prompt });
  }

  const menuItems: { action: SandboxRequestAction; label: string }[] = [];
  if (sandbox && writable) {
    if (ready || saveFailed) menuItems.push({ action: "sleep", label: saveFailed ? "Request retry sleep" : "Request sleep" });
    if (asleep || saveFailed) menuItems.push({ action: "wake", label: asleep ? "Request wake" : "Request resume" });
    if (!["deleting", "deleted"].includes(sandbox.phase)) {
      menuItems.push({ action: "command", label: "Ask to run a command" });
      menuItems.push({ action: "settings", label: "Request settings change" });
      menuItems.push({ action: "save", label: "Request a save" });
      if (ready) menuItems.push({ action: "preview", label: "Request a preview" });
      if (sandbox.desktop) menuItems.push({ action: "desktop", label: "Ask about the desktop" });
      menuItems.push({ action: "delete", label: "Request delete" });
    }
  }

  return <>
    {resource.error && !sandbox ? <div role="alert" className={styles.error}>{resource.error} <button onClick={resource.refresh}>Try again</button></div>
      : !sandbox ? <p role="status" className={styles.loading}>Loading…</p> : <>
      {resource.error ? <div role="alert" className={styles.error}>Couldn’t refresh this sandbox. Showing the last loaded state. {resource.error} <button onClick={resource.refresh}>Try again</button></div> : null}
      <div className={consoleStyles.workHeader} data-motion="reveal">
        <div className={consoleStyles.workTitle}>
          <h1>{sandbox.name}</h1>
          <p className={consoleStyles.workMeta}>
            <span className={styles.status} data-phase={ready ? "succeeded" : saveFailed ? "failed" : sandbox.phase}>{sandboxPhaseText(sandbox.phase)}</span>
            <span>{sandbox.size}</span>
            <span>{sandbox.tier !== "paid" ? "free tier" : asleep && sandbox.hasCheckpoint
              ? "Compute stopped · retained storage billed separately"
              : sandboxHourlyCents(sandbox.monthlyCents)}</span>
            {sandbox.tier === "paid" ? <span>${(sandbox.monthlyCents / 100).toFixed(2)} for 720 awake hours</span> : null}
            <span>{sandbox.idleTimeoutSeconds === undefined || sandbox.idleTimeoutSeconds === -1 ? "Organisation default idle sleep" : sandbox.idleTimeoutSeconds === 0 ? "No idle sleep" : `Sleeps after ${Math.round(sandbox.idleTimeoutSeconds / 60)} min idle`}</span>
            {sandbox.hasCheckpoint || sandbox.snapshotId ? <span>save available</span> : null}
            {sandbox.lastUsedAt ? <span>active {relativeTime(sandbox.lastUsedAt)}</span> : null}
            <code className={consoleStyles.workId}>{sandbox.id}</code>
          </p>
        </div>
        {writable ? <div className={consoleStyles.workActions}>
          <Link href="/app" className={styles.primaryButton}>Ask Canter</Link>
          {menuItems.length ? <Menu open={menuOpen} onOpenChange={setMenuOpen} onOpenChangeComplete={open => {
            // The request dialog must not race the menu's focus recovery: run
            // the chosen action only after the popup has fully closed.
            if (open || pendingAction.current === null) return;
            const action = pendingAction.current;
            pendingAction.current = null;
            ask(action);
          }}>
            <MenuTrigger ref={moreTrigger} className={styles.secondaryButton}>More <WorkspaceIcon name="down" width="13" height="13" /></MenuTrigger>
            <MenuContent className={`dashboard-theme ${consoleStyles.moreMenu}`} side="bottom" align="end" sideOffset={6}>
              {menuItems.map(item => <MenuItem key={item.action} value={item.action} label={item.label} destructive={item.action === "delete"} className={consoleStyles.moreItem} onSelect={() => { pendingAction.current = item.action; }}>{item.action === "delete" ? <span className={styles.status} data-phase="failed">{item.label}</span> : item.label}</MenuItem>)}
            </MenuContent>
          </Menu> : null}
        </div> : null}
      </div>
      {sandbox.failure ? <p role="alert" className={styles.error}>{sandbox.failure}</p> : null}
      {sandbox.labels && Object.keys(sandbox.labels).length ? <p className={styles.status}>{Object.entries(sandbox.labels).map(([key, value]) => `${key}=${value}`).join(" · ")}</p> : null}
      {saveFailed && !sandbox.failure ? <div role="alert" className={styles.error}>Saving files failed. Your VM and its files have been kept.</div> : null}
      {saveFailed && sandbox.tier === "paid" ? <p className={styles.loading}>Compute usage continues while this VM is running.</p> : null}

      {readable || asleep ? <Tabs value={activeSection} onValueChange={value => { if (value !== null) setSection(value); }}>
        <TabsList className={consoleStyles.sections} aria-label="Sandbox sections">
          <TabsTab value="overview">Overview</TabsTab>
          {readable ? <TabsTab value="files">Files</TabsTab> : null}
          {historyAvailable ? <TabsTab value="jobs">History</TabsTab> : null}
          {readable || asleep ? <TabsTab value="saves">Saves</TabsTab> : null}
          {desktop ? <TabsTab value="desktop">Desktop</TabsTab> : null}
        </TabsList>

      <TabsPanel value="overview">
        {ready && (metrics || metricsError) ? <>
          {metricsError && metrics ? <p className={styles.status} role="status">Metrics are stale — last successful read shown.</p> : null}
          {metrics ? <dl className={consoleStyles.facts}>
            <div><dt>CPU load (1 min)</dt><dd>{metrics.cpu.load1.toFixed(2)} <small>· {metrics.cpu.count} vCPUs</small></dd></div>
            <div><dt>Memory</dt><dd>{formatBytes(metrics.memory.usedBytes)} <small>/ {formatBytes(metrics.memory.totalBytes)}</small></dd></div>
            <div><dt>Disk</dt><dd>{formatBytes(metrics.disk.usedBytes)} <small>/ {formatBytes(metrics.disk.totalBytes)}</small></dd></div>
            <div><dt>Uptime</dt><dd>{Math.round(metrics.uptimeSeconds / 60)} min</dd></div>
          </dl> : <p className={styles.status}>{metricsError || "Metrics unavailable."}</p>}
        </> : null}
        {asleep ? <p className={styles.status}>Saved environments and retained job history remain available while asleep.</p> : null}
        {ready && previewsAvailable ? <section><h2>Previews</h2>
          <p className={styles.status}>Anyone with the link can open this port.</p>
          <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>{(previews ?? []).map(item => <li key={item.id} style={{ display: "flex", gap: 8, alignItems: "center", padding: "6px 0" }}>
            <code style={{ fontSize: 12 }}>:{item.port}</code>
            <a href={item.url} target="_blank" rel="noreferrer" className={styles.secondaryButton}>Open</a>
            <button className={styles.secondaryButton} onClick={() => void navigator.clipboard?.writeText(item.url)}>Copy link</button>
          </li>)}{previews?.length === 0 ? <li className={styles.status}>No active previews.</li> : null}</ul>
        </section> : null}
      </TabsPanel>

      {readable ? <TabsPanel value="files"><section><h2>Files <span className={styles.status}>{fileContent?.path ?? path}</span></h2>
        {filesError ? <p role="alert" className={styles.error}>{filesError}</p> : null}
        {!fileContent && path !== "/workspace" ? <button className={styles.secondaryButton} onClick={() => void browse(path.slice(0, path.lastIndexOf("/")) || "/workspace")}>Up</button> : null}
        {fileContent ? <div style={{ display: "flex", gap: 8, marginBottom: 8 }}>
          <button className={styles.secondaryButton} onClick={() => setFileContent(null)}>Back</button>
          <a className={styles.secondaryButton} href={sandboxDownloadURL(base, fileContent.path)} download>Download</a>
        </div> : null}
        {fileContent?.truncated ? <p className={styles.status}>Showing the beginning of this file. Download for the full contents.</p> : null}
        {fileContent ? <pre className={consoleStyles.pre}>{fileContent.content}</pre>
          : <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>{(entries ?? []).map(entry => <li key={entry.name} style={{ padding: "6px 0", display: "flex", gap: 8, alignItems: "center" }}><button className={styles.secondaryButton} onClick={() => void browse(path === "/" ? `/${entry.name}` : `${path}/${entry.name}`)}><WorkspaceIcon name={entry.directory ? "folder" : "file"} width="14" height="14" />{entry.name}</button>{!entry.directory ? <a className={styles.secondaryButton} href={sandboxDownloadURL(base, (path === "/" ? "/" : path + "/") + entry.name)} download style={{ fontSize: 12 }}>Download</a> : null}</li>)}{entries === null ? <li className={styles.status}>Loading…</li> : entries.length === 0 ? <li className={styles.status}>Empty directory</li> : null}</ul>}
      </section></TabsPanel> : null}

      {historyAvailable ? <TabsPanel value="jobs">
        <SandboxJobHistory base={base} workspaceId={workspaceId} sandboxId={id} owner={role === "owner"} selected={historyJob} onSelect={selectHistoryJob}
          onAsk={writable ? prompt => {
            if (onRequest) onRequest(prompt);
            else setDialog({ source: { accountId, workspaceId, workspaceName: workspaceDisplayName(workspaceName ?? "") }, prompt });
          } : undefined} />
        {readable ? <details><summary>Read a live job by ID</summary><section>
        <p className={styles.status}>Paste a job ID to read its log.</p>
        <form onSubmit={inspectJob} style={{ display: "flex", gap: 8, marginBottom: 12, flexWrap: "wrap" }}>
          <Input aria-label="Job ID" placeholder="job_…" value={jobId} onValueChange={setJobId} className={consoleStyles.jobInput} />
          <button className={styles.secondaryButton} type="submit" disabled={!jobId.trim()}>Read log</button>
        </form>
        {jobError ? <p role="alert" className={styles.error}>{jobError}</p> : null}
        {job ? <div>
          <span className={styles.status} data-phase={job.status === "completed" ? "succeeded" : job.status}>{job.status}{job.exitCode != null ? ` · exit ${job.exitCode}` : ""}</span>
          {job.error ? <span role="alert" className={styles.error}>{job.error}</span> : null}
          {job.truncated ? <span className={styles.status}> log truncated</span> : null}
          <pre className={consoleStyles.pre} style={{ marginTop: 6 }}>{job.text || (["starting", "running"].includes(job.status) ? "(waiting for output…)" : "(no output)")}</pre>
        </div> : null}
      </section></details> : null}</TabsPanel> : null}

      {readable || asleep ? <TabsPanel value="saves"><section>
        {asleep ? <p className={styles.status}>Saved environments and retained job history remain available while asleep.</p> : null}
        <SandboxSavedEnvironments key={`${sandbox.id}:${sandbox.generation}:${sandbox.phase}:${sandbox.snapshotId ?? ""}`} workspaceId={workspaceId} writer={false} sandbox={sandbox} />
      </section></TabsPanel> : null}

      {desktop ? <TabsPanel value="desktop"><SandboxDesktop key={`${sandbox.id}:${sandbox.generation}`} base={base} sandbox={sandbox} accountId={accountId} writer={false} /></TabsPanel> : null}
      </Tabs> : null}

      {sandbox.notice ? <p role="status" className={styles.loading}>{sandbox.notice}</p> : null}
    </>}
    {dialog && !onRequest ? <DashboardRequestDialog source={dialog.source} prompt={dialog.prompt} surface={sandbox ? { kind: "sandbox", id: sandbox.id } : undefined} onClose={() => setDialog(null)} /> : null}
  </>;
}
