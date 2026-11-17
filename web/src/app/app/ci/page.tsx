"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { Suspense, useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { JobLinkWorkspace } from "@/components/job-link-workspace";
import { AppShell } from "@/components/app-shell";
import { useWorkspace } from "@/components/workspace-context";
import { useRequestScope } from "@/components/use-request-scope";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { CanterAPIError, canterFetch, relativeTime, type Workspace } from "@/lib/canter-api";
import { formatBytes } from "@/lib/sandboxes";
import {
  ciJobActive, ciJobPhase, ciJobStateText, ciSizeLabel, ciStateText, featureStateText,
  filterCIFJobs, formatDurationSeconds, formatMoneyMicros, formatReportedDurationMs,
  formatRatePerMinute, freePeriodText, metricText, monthlyQuotaText, parseCount,
  parseSpendCapCents, poolStateText, runsOnLabel, safeGitHubURL, shortSha, sizeSummary,
  spendCapText, ciRunnerEvidenceRows, ciLogCollectionNotice, type CIJob, type CIJobDetail, type CIOverview, type CIPricingSize,
} from "@/lib/ci";
import styles from "@/components/workspace.module.css";
import ci from "./ci.module.css";
import { CiLogReader } from "./ci-log-reader";

export default function CiPage() {
  const { data, error, loading } = useWorkspace();
  const productName = data?.products?.find(product => product.id === "ci")?.name ?? "CI";
  if (!data) return <AppShell active="CI" pageTitle="CI"><section className={styles.contentPage}>
    <p role={error ? "alert" : "status"}>{loading ? "Loading CI…" : "Couldn’t load this organization."}</p>
  </section></AppShell>;
  return <Suspense fallback={<AppShell active="CI" pageTitle={productName}><p role="status">Loading CI…</p></AppShell>}><CiDashboard key={data.workspace.id} workspace={data.workspace} memberships={data.workspaces} productName={productName} /></Suspense>;
}

function CiDashboard({ workspace, memberships, productName }: { workspace: Workspace; memberships?: Workspace[]; productName: string }) {
  const { signal: requestSignal } = useRequestScope();
  const writer = workspace.role === "owner" || workspace.role === "operator";
  const owner = workspace.role === "owner";
  const [overview, setOverview] = useState<CIOverview | null>(null);
  const [loadError, setLoadError] = useState("");
  const params = useSearchParams();
  const selected = params.get("job");
  const requestedWorkspace = params.get("workspace");
  const linkedElsewhere = Boolean(selected && requestedWorkspace && requestedWorkspace !== workspace.id);
  const repository = params.get("repository") ?? "";
  const status = params.get("status") ?? "";
  const line = params.get("line");
  const anchor = line && /^\d{1,7}$/.test(line) ? Number(line) : undefined;
  function setUrl(changes: Record<string, string | null>) {
    const url = new URL(window.location.href);
    if ("job" in changes) changes.workspace = changes.job ? workspace.id : null;
    for (const [key, value] of Object.entries(changes)) {
      if (value) url.searchParams.set(key, value);
      else url.searchParams.delete(key);
    }
    if (url.href !== window.location.href) window.history.pushState(null, "", url);
  }
  const [jobFilter, setJobFilter] = useState("");
  const [actionError, setActionError] = useState("");
  const [connecting, setConnecting] = useState(false);
  const [connectError, setConnectError] = useState("");
  const loadSequence = useRef(0);
  const base = `/workspaces/${encodeURIComponent(workspace.id)}/ci`;

  const load = useCallback(async () => {
    const signal = requestSignal();
    if (signal.aborted || linkedElsewhere) return;
    const sequence = ++loadSequence.current;
    try {
      const next = await canterFetch<CIOverview>(base, { signal });
      if (signal.aborted || sequence !== loadSequence.current) return;
      setOverview(next);
      setLoadError("");
    } catch (cause) {
      if (!signal.aborted && sequence === loadSequence.current) setLoadError(cause instanceof Error ? cause.message : "Couldn’t load CI.");
    }
  }, [base, linkedElsewhere, requestSignal]);

  useEffect(() => {
    queueMicrotask(() => void load());
    const interval = window.setInterval(() => void load(), 10000);
    return () => window.clearInterval(interval);
  }, [load]);

  const applyJob = useCallback((updated: CIJob) => {
    setOverview(current => current ? { ...current, jobs: current.jobs.map(job => job.id === updated.id ? updated : job) } : current);
  }, []);

  async function connectGitHub() {
    const signal = requestSignal();
    if (signal.aborted || !owner || connecting) return;
    setConnecting(true);
    setConnectError("");
    try {
      const result = await canterFetch<{ url: string }>(`${base}/github/connect`, { method: "POST", signal });
      if (signal.aborted) return;
      const url = safeGitHubURL(result.url);
      if (!url) {
        setConnectError("The server returned an unexpected GitHub connect URL.");
        return;
      }
      window.location.assign(url);
    } catch (cause) {
      if (!signal.aborted) setConnectError(cause instanceof Error ? cause.message : "Couldn’t start the GitHub connection.");
    } finally {
      if (!signal.aborted) setConnecting(false);
    }
  }

  const jobs = overview?.jobs ?? [];
  const repositories = [...new Set(jobs.map(job => job.repository).filter(Boolean))].sort();
  const statuses = [...new Set(jobs.map(job => job.conclusion || job.status))].sort();
  const visibleJobs = filterCIFJobs(jobs, jobFilter).filter(job => (!repository || job.repository === repository) && (!status || (job.conclusion || job.status) === status));
  const pricing = overview?.pricing;
  const settings = overview?.settings;
  const features = overview?.features ?? [];
  const featureReady = (id: string) => features.some(feature => feature.id === id && feature.status === "available");
  const poolReady = overview?.pool.ready === true;
  const canCancel = featureReady("job-cancel");
  const githubAppReady = featureReady("github-app");
  const canAsk = featureReady("agent-inspect");
  const overrunReady = featureReady("paid-overrun");
  const cacheReady = featureReady("cache");
  if (linkedElsewhere && requestedWorkspace) return <AppShell active="CI" pageTitle={productName}><section className={styles.contentPage}>
    <JobLinkWorkspace id={requestedWorkspace} memberships={memberships} />
  </section></AppShell>;
  return <AppShell active="CI" pageTitle={productName}><section className={styles.contentPage}>
    <div className={styles.pageHeading} data-motion="reveal"><div><h1>{productName}</h1><p>GitHub Actions runners for this organization.</p></div></div>
    {loadError ? <div role="alert" className={styles.error}>{loadError} <button onClick={() => void load()}>Try again</button></div> : null}
    {!overview ? <p role="status" className={styles.loading}>Loading CI…</p> : !overview.enabled || !overview.installations.length ? <div className={styles.emptyState} data-motion="reveal"><span className={styles.emptyIcon}><WorkspaceIcon name="changes" width="24" height="24" /></span><h2>Run GitHub Actions on Canter</h2><p>Connect GitHub, then point a workflow at a Canter runner. {pricing?.freeSecondsPerRun ? `The first ${freePeriodText(pricing.freeSecondsPerRun)} of every run is free.` : "Every run starts with a free period."}</p>
      {owner ? <button type="button" className={styles.primaryButton} disabled={connecting || !githubAppReady} onClick={() => void connectGitHub()}>{connecting ? "Connecting…" : "Connect GitHub"}</button> : <p className={styles.status}>Ask an owner to connect GitHub.</p>}
      {owner && !githubAppReady ? <p className={styles.status}>GitHub connections aren’t available yet.</p> : null}
      {connectError ? <p role="alert" className={styles.error}>{connectError}</p> : null}
    </div> : <>
      {actionError ? <p role="alert" className={styles.error}>{actionError}</p> : null}
      <div className={ci.statGrid}>
        <div className={ci.statCard}><h2>Runner pool</h2><p>{poolStateText(overview.pool)}{overview.pool.ready ? "" : overview.pool.message ? ` — ${overview.pool.message}` : ""}</p></div>
        <div className={ci.statCard}><h2>Free usage</h2><p>First {freePeriodText(pricing!.freeSecondsPerRun)} of execution free on every runner. {monthlyQuotaText(pricing!.monthlyMinuteLimit)}</p></div>
        <div className={ci.statCard}><h2>Sizes</h2><p>{pricing!.sizes.length} launch {pricing!.sizes.length === 1 ? "size" : "sizes"} · up to {pricing!.maxVcpus} vCPU</p></div>
        <div className={ci.statCard}><h2>Billing</h2><p>{settings!.paidOverrunEnabled ? `Paid overrun on · cap $${spendCapText(settings!.monthlySpendLimitCents)}/month` : "Paid overrun off — free period only"}</p></div>
      </div>
      {!poolReady ? <p role="alert" className={styles.error}>CI runners are not activated yet — no jobs will run. Don&apos;t migrate required checks until the pool shows Ready.</p> : null}
      <div className={ci.layout}>
        <section aria-label="Recent jobs">
          <div className={ci.jobsHeader}><h2>Recent jobs</h2><input className={ci.filterInput} aria-label="Filter recent jobs" placeholder="Filter jobs…" value={jobFilter} onChange={event => setJobFilter(event.target.value)} /></div>
          <div className={ci.jobFilters}>
            <select className={ci.filterInput} aria-label="Repository" value={repository} onChange={event => setUrl({ repository: event.target.value })}>
              <option value="">All repositories</option>
              {repository && !repositories.includes(repository) ? <option value={repository}>{repository}</option> : null}
              {repositories.map(name => <option key={name} value={name}>{name}</option>)}
            </select>
            <select className={ci.filterInput} aria-label="Status" value={status} onChange={event => setUrl({ status: event.target.value })}>
              <option value="">All statuses</option>
              {status && !statuses.includes(status) ? <option value={status}>{ciStateText(status)}</option> : null}
              {statuses.map(name => <option key={name} value={name}>{ciStateText(name)}</option>)}
            </select>
          </div>
          {visibleJobs.length ? <ul className={ci.jobList}>{visibleJobs.map(job => <li key={job.id}><button type="button" className={ci.jobButton} aria-pressed={selected === job.id} onClick={() => setUrl({ job: selected === job.id ? null : job.id, line: null })}>
            <span className={ci.jobTitle}>{job.name || job.workflowName}</span>
            <span className={ci.jobMeta}>{job.repository} · {job.ref}</span>
            <span className={styles.status} data-phase={ciJobPhase(job)}>{ciJobStateText(job)}</span>
            <span className={ci.jobMeta}>{relativeTime(job.createdAt)} · {formatDurationSeconds(job.elapsedSeconds)} elapsed · {formatMoneyMicros(job.costMicros, pricing!.currency)}</span>
          </button></li>)}</ul> : <p className={styles.status}>{jobs.length ? "No jobs match the filter." : "No jobs yet — runs appear here after a workflow uses a Canter CI runner."}</p>}
          <p className={`${ci.jobMeta} ${ci.jobMetaWrap}`}>Private repository jobs appear only for members whose own GitHub account can access the repository.</p>
        </section>
        <section className={ci.detail} aria-label="Job details">
          {selected ? <CiJobDetail key={selected} base={base} workspaceId={workspace.id} jobId={selected} anchor={anchor} onAnchor={offset => setUrl({ line: String(offset) })} currency={pricing!.currency} sizes={pricing!.sizes} writer={writer} canAsk={canAsk} canCancel={canCancel} onJobUpdate={applyJob} onError={setActionError} /> : <p className={ci.detailEmpty}>Select a job to see its steps, logs, and results.</p>}
        </section>
      </div>
      <section className={ci.section}><h2>Runner sizes</h2>
        <p>Each runner&apos;s first {freePeriodText(pricing!.freeSecondsPerRun)} of execution is free for every job — repeated jobs are allowed. {monthlyQuotaText(pricing!.monthlyMinuteLimit)} The overrun rate applies to seconds beyond the free period. Billing is owner opt-in and requires pay-as-you-go billing plus a monthly spend cap.</p>
        {!poolReady ? <p className={ci.jobMeta}>Runner labels and rates are reserved until the pool is ready.</p> : null}
        <div className={ci.sizeGrid}>{pricing!.sizes.map(size => <div key={size.id} className={ci.sizeCard}>
          <h3>{size.label}</h3>
          <p>{sizeSummary(size)} · {formatRatePerMinute(size.microsPerMinute, pricing!.currency)} overrun · Blacksmith {formatRatePerMinute(size.blacksmithMicrosPerMinute, pricing!.currency)}</p>
          <span className={ci.runsOn}><code>runs-on: {runsOnLabel(size)}</code><CopyButton text={runsOnLabel(size)} label={`Copy runs-on label for ${size.label}`} disabled={!poolReady} /></span>
        </div>)}</div>
        {/^https:\/\//.test(pricing!.referenceUrl) ? <p><a href={pricing!.referenceUrl} target="_blank" rel="noreferrer">Blacksmith pricing reference</a></p> : null}
      </section>
      {overview.features.length ? <section className={ci.section}><h2>Features</h2>
        <ul className={ci.featureList}>{overview.features.map(feature => <li key={feature.id} className={ci.featureRow}>
          <span className={ci.featureName}>{feature.name}</span>
          <span className={styles.status}>{featureStateText(feature.status)}</span>
          <span className={ci.featureDesc}>{feature.description}</span>
        </li>)}</ul>
      </section> : null}
      <section className={ci.section}><h2>GitHub</h2>
        {overview.installations.length ? <ul className={ci.installList}>{overview.installations.map(installation => <li key={installation.id} className={ci.installRow}>
          <span className={ci.installName}>{installation.owner}</span>
          <span className={styles.status}>{ciStateText(installation.status)}</span>
          <span className={ci.installMeta}>{installation.repositoryCount} {installation.repositoryCount === 1 ? "repository" : "repositories"}</span>
        </li>)}</ul> : <p>No GitHub installations connected.{owner ? "" : " Ask an organization owner to connect one."}</p>}
        {owner ? <p><button type="button" className={styles.secondaryButton} disabled={connecting || !githubAppReady} onClick={() => void connectGitHub()}>{connecting ? "Connecting…" : overview.installations.length ? "Connect another GitHub organization" : "Connect GitHub"}</button></p> : null}
        {connectError ? <p role="alert" className={styles.error}>{connectError}</p> : null}
      </section>
      {!writer ? <p className={ci.section}>You have read-only access to this organization.</p> : null}
      {owner && settings ? <CiSettings key={JSON.stringify(settings)} base={base} settings={settings} sizes={pricing!.sizes} overrunReady={overrunReady} cacheReady={cacheReady} onSaved={next => setOverview(current => current ? { ...current, settings: next } : current)} /> : null}
    </>}
  </section></AppShell>;
}

function CopyButton({ text, label, disabled = false }: { text: string; label: string; disabled?: boolean }) {
  const [copied, setCopied] = useState(false);
  return <button type="button" className={ci.copyButton} aria-label={label} disabled={disabled} title={disabled ? "Available when the runner pool is ready" : undefined} onClick={() => {
    void navigator.clipboard?.writeText(text).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    }).catch(() => {});
  }}>{copied ? "Copied" : "Copy"}</button>;
}

function CiJobDetail({ base, workspaceId, jobId, anchor, onAnchor, currency, sizes, writer, canAsk, canCancel, onJobUpdate, onError }: { base: string; workspaceId: string; jobId: string; anchor?: number; onAnchor: (offset: number) => void; currency: string; sizes: CIPricingSize[]; writer: boolean; canAsk: boolean; canCancel: boolean; onJobUpdate: (job: CIJob) => void; onError: (message: string) => void }) {
  const { signal: requestSignal } = useRequestScope();
  const [detail, setDetail] = useState<CIJobDetail | null>(null);
  const [loadError, setLoadError] = useState("");
  const [cancelling, setCancelling] = useState(false);
  const sequence = useRef(0);

  const load = useCallback(async () => {
    const signal = requestSignal();
    if (signal.aborted) return;
    const current = ++sequence.current;
    try {
      const value = await canterFetch<CIJobDetail>(`${base}/jobs/${encodeURIComponent(jobId)}?logs=range`, { signal });
      if (signal.aborted || current !== sequence.current || value.job.id !== jobId) return;
      setDetail(value);
      setLoadError("");
    } catch (cause) {
      if (signal.aborted || current !== sequence.current) return;
      if (cause instanceof CanterAPIError && [401, 403, 404].includes(cause.status)) setDetail(null);
      setLoadError(cause instanceof Error ? cause.message : "Couldn’t load this job.");
    }
  }, [base, jobId, requestSignal]);

  useEffect(() => {
    queueMicrotask(() => void load());
    const interval = window.setInterval(() => { if (!document.hidden) void load(); }, 10000);
    return () => window.clearInterval(interval);
  }, [load]);

  async function cancel() {
    const signal = requestSignal();
    if (signal.aborted || !writer || cancelling || !detail || !ciJobActive(detail.job)) return;
    setCancelling(true);
    onError("");
    try {
      const result = await canterFetch<{ job: CIJob }>(`${base}/jobs/${encodeURIComponent(jobId)}/cancel`, { method: "POST", signal });
      if (signal.aborted) return;
      setDetail(current => current ? { ...current, job: result.job } : current);
      onJobUpdate(result.job);
    } catch (cause) {
      if (!signal.aborted) onError(cause instanceof Error ? cause.message : "Couldn’t cancel this job.");
    } finally {
      if (!signal.aborted) setCancelling(false);
    }
  }

  if (loadError && !detail) return <div role="alert" className={styles.error}>{loadError} <button onClick={() => void load()}>Try again</button></div>;
  if (!detail) return <p role="status" className={styles.loading}>Loading job…</p>;
  const { job, steps, logs, metrics, tests, runnerDiagnostics, logCollection, rangeLogs, pinnedRate } = detail;
  const githubURL = safeGitHubURL(job.githubURL);
  const askPrompt = `Inspect Canter CI job ${job.id} in this organization. Explain the result using its logs; do not make changes.`;
  const orderedLogs = [...logs].sort((a, b) => a.sequence - b.sequence);
  const streams = new Map<string, string[]>();
  for (const entry of orderedLogs) {
    const bucket = streams.get(entry.stream) ?? [];
    bucket.push(entry.text);
    streams.set(entry.stream, bucket);
  }
  const latest = metrics.length ? metrics[metrics.length - 1] : null;
  const samples = metrics.slice(-10).reverse();
  const active = ciJobActive(job);
  const runnerRows = ciRunnerEvidenceRows(runnerDiagnostics);
  const logNotice = ciLogCollectionNotice(logCollection);
  return <>
    <div className={ci.detailHeader}><h2>{job.name || job.workflowName}</h2>
      <div className={ci.detailActions}>
        <span className={styles.status} data-phase={ciJobPhase(job)}>{ciJobStateText(job)}</span>
        {githubURL ? <a className={styles.secondaryButton} href={githubURL} target="_blank" rel="noreferrer">GitHub</a> : job.githubURL ? <span className={ci.jobMeta}>{job.githubURL}</span> : null}
        {writer && canAsk ? <Link className={styles.secondaryButton} href={`/app?compose=1&prompt=${encodeURIComponent(askPrompt)}`}>Ask Canter</Link> : null}
        {writer && canCancel && active ? <button type="button" className={styles.secondaryButton} disabled={cancelling} onClick={() => void cancel()}>{cancelling ? "Cancelling…" : "Cancel job"}</button> : null}
      </div></div>
    {job.queueReason ? <p className={ci.jobMeta}>{job.queueReason}</p> : null}
    {active && !canCancel ? <p className={ci.jobMeta}>Cancel this job in GitHub — Canter-side cancellation is not activated.</p> : null}
    <dl className={ci.detailMeta}>
      <div><dt>Repository</dt><dd>{job.repository}</dd></div>
      <div><dt>Workflow</dt><dd>{job.workflowName}</dd></div>
      <div><dt>Ref</dt><dd>{job.ref}</dd></div>
      <div><dt>Commit</dt><dd><code>{shortSha(job.headSha)}</code></dd></div>
      <div><dt>Size</dt><dd>{ciSizeLabel(sizes, job.size)}</dd></div>
      {job.runnerId ? <div><dt>Runner</dt><dd>{job.runnerId}</dd></div> : null}
      <div><dt>Created</dt><dd>{relativeTime(job.createdAt)}</dd></div>
      {job.startedAt ? <div><dt>Started</dt><dd>{relativeTime(job.startedAt)}</dd></div> : null}
      {job.completedAt ? <div><dt>Completed</dt><dd>{relativeTime(job.completedAt)}</dd></div> : null}
    </dl>
    <p className={ci.jobMeta}>{formatDurationSeconds(job.elapsedSeconds)} elapsed · {formatDurationSeconds(job.billableSeconds)} billable · {formatMoneyMicros(job.costMicros, currency)}</p>
    {pinnedRate && pinnedRate.microsPerMinute ? <p className={ci.logNotice}>Job overrun rate {formatRatePerMinute(pinnedRate.microsPerMinute, currency)}</p> : null}
    {loadError ? <p role="alert" className={styles.error}>Showing previously loaded job details. {loadError}</p> : null}
    {rangeLogs ? <CiLogReader workspaceId={workspaceId} endpoint={`${base}/jobs/${encodeURIComponent(jobId)}/logs`} anchor={anchor} onAnchor={onAnchor} active={active} collection={logCollection} githubURL={githubURL} /> : null}
    <details className={`${ci.detailSection} ${ci.runnerDisclosure}`}><summary>Runner lifecycle</summary>
      {runnerDiagnostics ? <>
        {runnerDiagnostics.association === "provisioning_attempt" ? <p className={ci.jobMeta}>This provisioning attempt has not been assigned to this job.</p> : null}
        <dl className={ci.detailMeta}>{runnerRows.map(row => <div key={row.label}><dt>{row.label}</dt><dd>{row.value}</dd></div>)}</dl>
        <p className={ci.jobMeta}>A stop request is not proof that execution stopped or resources were deleted.</p>
      </> : <p className={ci.jobMeta}>No managed runner evidence recorded for this job.</p>}
    </details>
    {steps.length ? <section className={ci.detailSection}><h3>Steps</h3>
      <ul className={ci.stepList}>{steps.map(step => <li key={step.number} className={ci.stepRow}>
        <span className={ci.stepName}>{step.number}. {step.name}</span>
        <span className={styles.status} data-phase={step.status === "completed" ? (step.conclusion === "success" || step.conclusion === "skipped" ? "succeeded" : "failed") : "running"}>{step.conclusion ? ciStateText(step.conclusion) : ciStateText(step.status)}</span>
        <span className={ci.stepMeta}>{step.startedAt ? relativeTime(step.startedAt) : ""}{step.completedAt ? ` → ${relativeTime(step.completedAt)}` : ""}</span>
      </li>)}</ul>
    </section> : null}
    {tests.length ? <section className={ci.detailSection}><h3>Tests</h3>
      {tests.some(test => test.source === "workflow") ? <p className={ci.jobMeta}>Test results are reported by the workflow and do not replace the GitHub job conclusion.</p> : null}
      <ul className={ci.testList}>{tests.map(test => <li key={test.id} className={ci.testRow}>
        <span className={ci.testName}>{test.name}{test.suite ? <span className={ci.jobMeta}> · {test.suite}</span> : null}</span>
        <span className={styles.status} data-phase={test.status === "passed" || test.status === "success" ? "succeeded" : test.status === "failed" || test.status === "failure" ? "failed" : "drafted"}>{ciStateText(test.status)}</span>
        <span className={ci.testMeta}>{formatReportedDurationMs(test.durationMs)}</span>
        {test.message ? <p className={ci.testMessage}>{test.message}</p> : null}
      </li>)}</ul>
    </section> : null}
    {metrics.length ? <section className={ci.detailSection}><h3>Resources</h3>
      {metrics.some(metric => metric.source === "guest") ? <p className={ci.jobMeta}>Guest-reported VM usage; samples do not determine billing.</p> : null}
      {latest ? <div className={ci.metricGrid}>
        <div className={ci.metric}><span>CPU</span><strong>{metricText(latest.cpuPercent, value => `${value}%`)}</strong></div>
        <div className={ci.metric}><span>Memory</span><strong>{metricText(latest.memoryBytes, formatBytes)}{latest.memoryLimitBytes != null ? ` / ${formatBytes(latest.memoryLimitBytes)}` : ""}</strong></div>
        <div className={ci.metric}><span>Network in</span><strong>{metricText(latest.networkRxBytes, formatBytes)}</strong></div>
        <div className={ci.metric}><span>Network out</span><strong>{metricText(latest.networkTxBytes, formatBytes)}</strong></div>
        <div className={ci.metric}><span>OOM kills</span><strong>{metricText(latest.oomKills, value => String(value))}</strong></div>
      </div> : null}
      {samples.length > 1 ? <div className={ci.samplesWrap}><table className={ci.samples}><thead><tr><th>Time</th><th>CPU</th><th>Memory</th><th>Net in</th><th>Net out</th><th>OOM</th></tr></thead>
        <tbody>{samples.map((sample, index) => <tr key={`${sample.at}-${index}`}><td>{relativeTime(sample.at)}</td><td>{metricText(sample.cpuPercent, value => `${value}%`)}</td><td>{metricText(sample.memoryBytes, formatBytes)}</td><td>{metricText(sample.networkRxBytes, formatBytes)}</td><td>{metricText(sample.networkTxBytes, formatBytes)}</td><td>{metricText(sample.oomKills, value => String(value))}</td></tr>)}</tbody></table></div> : null}
    </section> : null}
    {!rangeLogs ? <section className={ci.detailSection}><h3>Logs</h3>
      {logNotice ? <p className={ci.jobMeta}>{logNotice}</p> : null}
      {streams.size ? [...streams.entries()].map(([stream, chunks]) => <div key={stream}>
        <p className={ci.logLabel}>{stream === "stdout" ? "Standard output" : stream === "stderr" ? "Standard error" : ciStateText(stream)}</p>
        <pre className={ci.log}>{chunks.join("") || "(no output)"}</pre>
      </div>) : <p className={ci.jobMeta}>No logs yet.</p>}
    </section> : null}
  </>;
}

function CiSettings({ base, settings, sizes, overrunReady, cacheReady, onSaved }: { base: string; settings: CIOverview["settings"]; sizes: CIPricingSize[]; overrunReady: boolean; cacheReady: boolean; onSaved: (settings: CIOverview["settings"]) => void }) {
  const { signal: requestSignal } = useRequestScope();
  const [draft, setDraft] = useState<{ paidOverrunEnabled?: boolean; spendCap?: string; cacheEnabled?: boolean; retainFailedMinutes?: string; defaultSize?: string }>({});
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const [saved, setSaved] = useState(false);
  const paidOverrunEnabled = draft.paidOverrunEnabled ?? settings.paidOverrunEnabled;
  const spendCap = draft.spendCap ?? spendCapText(settings.monthlySpendLimitCents);
  const cacheEnabled = draft.cacheEnabled ?? settings.cacheEnabled;
  const retainFailedMinutes = draft.retainFailedMinutes ?? String(settings.retainFailedMinutes);
  const defaultSize = draft.defaultSize ?? settings.defaultSize;

  async function save(event: FormEvent) {
    event.preventDefault();
    const signal = requestSignal();
    if (pending || signal.aborted) return;
    const capCents = parseSpendCapCents(spendCap);
    if (capCents == null) { setError("Spend cap must be a dollar amount from $0.01 to $1000.00."); return; }
    const retain = parseCount(retainFailedMinutes);
    if (retain == null) { setError("Retain failed minutes must be a whole number of minutes."); return; }
    setPending(true);
    setError("");
    setSaved(false);
    try {
      const body = JSON.stringify({ paidOverrunEnabled, monthlySpendLimitCents: capCents, cacheEnabled, retainFailedMinutes: retain, defaultSize });
      const result = await canterFetch<{ settings: CIOverview["settings"] }>(`${base}/settings`, { method: "PATCH", body, signal });
      if (signal.aborted) return;
      onSaved(result.settings);
      setDraft({});
      setSaved(true);
    } catch (cause) { if (!signal.aborted) setError(cause instanceof Error ? cause.message : "Couldn’t save CI settings."); }
    finally { if (!signal.aborted) setPending(false); }
  }

  return <section className={ci.section}><h2>Settings</h2>
    <form className={ci.settingsForm} onSubmit={save}>
      <label className={ci.checkField}><input type="checkbox" checked={paidOverrunEnabled} disabled={pending || !overrunReady} onChange={event => { setDraft(current => ({ ...current, paidOverrunEnabled: event.target.checked })); setSaved(false); }} />Paid overrun — bill seconds beyond the free period{overrunReady ? "" : " (not activated)"}</label>
      <label className={ci.field}>Monthly spend cap (USD)<input inputMode="decimal" value={spendCap} disabled={pending} onChange={event => { setDraft(current => ({ ...current, spendCap: event.target.value })); setSaved(false); }} /></label>
      <label className={ci.checkField}><input type="checkbox" checked={cacheEnabled} disabled={pending || !cacheReady} onChange={event => { setDraft(current => ({ ...current, cacheEnabled: event.target.checked })); setSaved(false); }} />Cache dependencies between runs{cacheReady ? "" : " (not activated)"}</label>
      <label className={ci.field}>Retain failed minutes<input inputMode="numeric" value={retainFailedMinutes} disabled title="Failed-run retention is not activated" onChange={event => { setDraft(current => ({ ...current, retainFailedMinutes: event.target.value.replace(/\D/g, "") })); setSaved(false); }} /></label>
      <label className={ci.field}>Default runner size<select value={defaultSize} disabled={pending} onChange={event => { setDraft(current => ({ ...current, defaultSize: event.target.value })); setSaved(false); }}>
        {sizes.map(size => <option key={size.id} value={size.id}>{size.label} — {sizeSummary(size)}</option>)}
        {!sizes.some(size => size.id === defaultSize) ? <option value={defaultSize}>{defaultSize}</option> : null}
      </select></label>
      <div style={{ display: "flex", gap: 10, alignItems: "center" }}>
        <button type="submit" className={styles.primaryButton} disabled={pending}>{pending ? "Saving…" : "Save settings"}</button>
        {saved ? <span className={styles.status}>Saved.</span> : null}
      </div>
      {error ? <p role="alert" className={styles.error}>{error}</p> : null}
    </form></section>;
}
