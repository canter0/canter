"use client";

import { useCallback, useEffect, useState } from "react";
import { CanterAPIError, canterFetch } from "@/lib/canter-api";
import { formatBytes } from "@/lib/sandboxes";
import styles from "./workspace.module.css";
import consoleStyles from "./sandbox-console.module.css";

type Job = {
  jobId: string; kind: string; generation?: number; actorKind?: string; actorId?: string;
  startedAt: string; finishedAt?: string; observedAt?: string; status: string; stale: boolean;
  exitCode?: number; outputState: string; retainedBytes: number;
};
type History = { jobs: Job[]; more: boolean; collectorEnabled: boolean; retentionEnabled: boolean; retentionDays: number; jobByteLimit: number };
type Output = { job: Job; dataBase64: string; nextOffset: number };

const outputLabels: Record<string, string> = {
  collecting: "Collecting output", complete: "Collected", incomplete: "Incomplete output",
  truncated: "Truncated output", expired: "Output expired", unavailable: "Output unavailable", not_collected: "Output was not collected",
};
const isDenied = (cause: unknown) => cause instanceof CanterAPIError && [401, 403, 404].includes(cause.status);
const message = (cause: unknown) => cause instanceof Error ? cause.message : "Couldn’t refresh job history.";

function result(job: Job) {
  if (job.exitCode != null) return `Exit ${job.exitCode}`;
  if (job.stale) return "Unknown";
  return job.status.replaceAll("_", " ");
}

export function SandboxJobHistory({ base, workspaceId, sandboxId, owner, selected, onSelect, onAsk }: {
  base: string; workspaceId: string; sandboxId: string; owner: boolean; selected: string;
  onSelect: (id: string) => void; onAsk?: (prompt: string) => void;
}) {
  const [history, setHistory] = useState<History | null>(null);
  const [loaded, setLoaded] = useState<{ job: Job; text: string } | null>(null);
  const [listError, setListError] = useState("");
  const [outputError, setOutputError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [saving, setSaving] = useState(false);
  const [copyStatus, setCopyStatus] = useState("");
  const job = loaded?.job.jobId === selected ? loaded.job : null;

  const loadList = useCallback(async (signal: AbortSignal) => {
    try {
      const value = await canterFetch<History>(`${base}/history`, { signal });
      if (!signal.aborted) { setHistory(value); setListError(""); }
    } catch (cause) {
      if (signal.aborted) return;
      if (isDenied(cause)) { setHistory(null); setLoaded(null); }
      setListError(message(cause));
    }
  }, [base]);

  useEffect(() => {
    const controller = new AbortController();
    window.queueMicrotask(() => { if (!controller.signal.aborted) void loadList(controller.signal); });
    const interval = window.setInterval(() => { if (!document.hidden) void loadList(controller.signal); }, 6000);
    return () => { controller.abort(); window.clearInterval(interval); };
  }, [loadList, refresh]);

  const selectedSummary = history?.jobs.find(item => item.jobId === selected);
  const selectedBytes = selectedSummary?.retainedBytes;
  const selectedState = selectedSummary?.outputState;
  const selectedStale = selectedSummary?.stale;
  useEffect(() => {
    if (!selected) return;
    const controller = new AbortController();
    const signal = controller.signal;
    async function read() {
      const endpoint = `${base}/history/${encodeURIComponent(selected)}`;
      try {
        const first = await canterFetch<Output>(endpoint, { signal });
        const limit = first.job.retainedBytes;
        const decoder = new TextDecoder();
        const bytes = (value: string) => Uint8Array.from(atob(value), char => char.charCodeAt(0));
        let text = decoder.decode(bytes(first.dataBase64), { stream: true });
        for (let offset = first.nextOffset; offset < limit;) {
          const chunk = await canterFetch<Output>(`${endpoint}?offset=${offset}`, { signal });
          if (chunk.nextOffset <= offset || chunk.job.outputState === "expired") {
            setLoaded({ job: chunk.job, text: "" });
            return;
          }
          text += decoder.decode(bytes(chunk.dataBase64), { stream: true });
          offset = chunk.nextOffset;
        }
        if (!signal.aborted) {
          setLoaded({ job: first.job, text: text + decoder.decode() });
          setOutputError("");
        }
      } catch (cause) {
        if (signal.aborted) return;
        if (isDenied(cause)) setLoaded(null);
        setOutputError(message(cause));
      }
    }
    void read();
    const interval = window.setInterval(() => { if (!document.hidden) void read(); }, 12000);
    return () => { controller.abort(); window.clearInterval(interval); };
  }, [base, selected, selectedBytes, selectedState, selectedStale, refresh]);

  async function changeRetention() {
    if (!history || saving) return;
    setSaving(true);
    try {
      await canterFetch(`${base}/history/retention`, { method: "PATCH", body: JSON.stringify({ retainOutput: !history.retentionEnabled }) });
      setRefresh(value => value + 1);
    } catch (cause) {
      setListError(message(cause));
    } finally {
      setSaving(false);
    }
  }

  function copyLink() {
    const url = new URL(`/app/sandboxes/${encodeURIComponent(sandboxId)}`, window.location.origin);
    url.searchParams.set("workspace", workspaceId);
    url.searchParams.set("historyJob", selected);
    void navigator.clipboard.writeText(url.href).then(() => setCopyStatus("Link copied")).catch(() => setCopyStatus("Couldn’t copy the link"));
  }

  return <section aria-label="Job history">
    <h2>History</h2>
    {history ? <p className={consoleStyles.historyNotice}>{history.collectorEnabled && history.retentionEnabled
      ? `Canter retains up to ${formatBytes(history.jobByteLimit)} of output per job for ${history.retentionDays} days. Output can contain sensitive information.`
      : "New output is not being retained. Previously retained output follows its original expiry."}
      {owner && history.collectorEnabled ? <button type="button" className={styles.secondaryButton} disabled={saving} onClick={() => void changeRetention()}>{history.retentionEnabled ? "Stop retaining output" : "Retain output for new jobs"}</button> : null}
    </p> : null}
    {listError ? <p role="alert" className={styles.error}>{history ? "Showing the last loaded list. " : ""}{listError} <button onClick={() => setRefresh(value => value + 1)}>Retry</button></p> : null}
    {history?.jobs.length ? <ul className={consoleStyles.historyList}>{history.jobs.map(item => <li key={item.jobId}>
      <button type="button" className={consoleStyles.historyRow} aria-pressed={selected === item.jobId} onClick={() => { setCopyStatus(""); setOutputError(""); onSelect(item.jobId); }}>
        <span>{item.kind === "setup" ? "Initial setup" : "Command job"}<small>{item.jobId}</small></span>
        <span>{result(item)}</span>
        <span>{outputLabels[item.outputState] ?? item.outputState}</span>
      </button>
    </li>)}</ul> : <p className={styles.status}>{history ? "No recorded jobs." : listError ? "" : "Loading job history…"}</p>}
    {history?.more ? <p className={consoleStyles.historyNotice}>Showing the 100 most recent jobs. Older jobs can still be opened by link.</p> : null}
    {selected ? <section className={consoleStyles.historyDetail} aria-label="Selected job">
      {outputError ? <p role="alert" className={styles.error}>{job ? "Showing the last loaded output. " : ""}{outputError} <button onClick={() => setRefresh(value => value + 1)}>Retry</button></p> : null}
      {job ? <>
        <h3>{job.kind === "setup" ? "Initial setup" : "Command job"}</h3>
        <p className={consoleStyles.historyNotice}><code>{job.jobId}</code> · {result(job)}{job.generation != null ? ` · generation ${job.generation}` : ""}</p>
        {job.actorKind ? <p className={consoleStyles.historyNotice}>Started by {job.actorKind}{job.actorId ? ` · ${job.actorId}` : ""}</p> : null}
        <p className={consoleStyles.historyNotice}>Recorded {new Date(job.startedAt).toLocaleString()}{job.finishedAt ? ` · finished ${new Date(job.finishedAt).toLocaleString()}` : ""}</p>
        {job.stale && job.status !== "unknown" ? <p className={consoleStyles.historyNotice}>Last observed {job.status}. The current result is unknown.</p> : null}
        <p role="status" className={consoleStyles.historyNotice}>{outputLabels[job.outputState]}{job.retainedBytes ? ` · ${formatBytes(job.retainedBytes)} retained` : ""}</p>
        {loaded?.text ? <pre className={consoleStyles.pre}>{loaded.text}</pre> : null}
        <div className={consoleStyles.historyActions}>
          <button type="button" className={styles.secondaryButton} onClick={copyLink}>Copy job link</button>
          <span role="status" className={consoleStyles.historyNotice}>{copyStatus}</span>
          {onAsk ? <button type="button" className={styles.secondaryButton} onClick={() => onAsk(`Inspect retained history for job ${job.jobId} in sandbox ${sandboxId}, organization ${workspaceId}. Use the Sandbox history output tool. Explain the recorded result and output.`)}>Ask Canter about this job</button> : null}
        </div>
      </> : !outputError ? <p role="status" className={consoleStyles.historyNotice}>Loading job details…</p> : null}
    </section> : null}
  </section>;
}
