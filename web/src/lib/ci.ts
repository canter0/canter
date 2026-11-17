export type CIPool = { ready: boolean; status: string; message: string };
export type CIPricingSize = { id: string; label: string; vcpus: number; memoryMiB: number; microsPerMinute: number; blacksmithMicrosPerMinute: number };
export type CIPricing = { version: string; currency: string; freeSecondsPerRun: number; monthlyMinuteLimit: number | null; maxVcpus: number; referenceUrl: string; sizes: CIPricingSize[] };
export type CISettings = { paidOverrunEnabled: boolean; monthlySpendLimitCents: number; cacheEnabled: boolean; retainFailedMinutes: number; defaultSize: string };
export type CIInstallation = { id: string; installationId: number; owner: string; status: string; repositoryCount: number };
export type CIJob = { id: string; repository: string; name: string; workflowName: string; ref: string; headSha: string; githubJobId: number; githubRunId: number; githubURL: string; status: string; conclusion?: string; size: string; createdAt: string; startedAt?: string; completedAt?: string; elapsedSeconds: number; billableSeconds: number; costMicros: number; queueReason: string; runnerId?: string };
export type CIFeature = { id: string; name: string; status: string; description: string };
export type CIOverview = { enabled: boolean; pool: CIPool; pricing: CIPricing; settings: CISettings; installations: CIInstallation[]; jobs: CIJob[]; features: CIFeature[] };
export type CIStep = { name: string; number: number; status: string; conclusion?: string; startedAt?: string; completedAt?: string };
export type CILog = { sequence: number; stream: string; text: string; createdAt: string };
export type CIMetric = { at: string; source?: string; cpuPercent: number | null; memoryBytes: number | null; memoryLimitBytes: number | null; networkRxBytes: number | null; networkTxBytes: number | null; oomKills: number | null };
export type CITestResult = { id: string; name: string; suite: string; status: string; durationMs: number | null; message: string; source?: string };
export type CIRunnerDiagnostics = {
  schemaVersion: number;
  runnerId: string;
  association: string;
  rateVersion?: string;
  rateMicrosPerMinute?: number;
  phase: string;
  createdAt: string;
  updatedAt: string;
  deleteRequested: boolean;
  createAttemptedAt: string | null;
  createSettled: boolean | null;
  setupDeadlineAt: string | null;
  hardDeadlineAt: string | null;
  executionStartedAt: string | null;
  executionStoppedAt: string | null;
  deadlineAt: string | null;
  stopRequestedAt: string | null;
  stopReason: string | null;
  billingStopAt: string | null;
  stoppedVerifiedAt: string | null;
  cleanupVerifiedAt: string | null;
};
export type CILogCollection = { state: string; truncated: boolean; collectedAt?: string | null };
export type CIJobDetail = { job: CIJob; rangeLogs?: boolean; steps: CIStep[]; logs: CILog[]; metrics: CIMetric[]; tests: CITestResult[]; runnerDiagnostics?: CIRunnerDiagnostics | null; logCollection?: CILogCollection | null };

function humanize(value: string) {
  const text = value.replace(/_/g, " ").trim();
  return text ? text[0].toUpperCase() + text.slice(1) : "Unknown";
}

const STATE_LABELS: Record<string, string> = {
  queued: "Queued", in_progress: "Running", waiting: "Waiting", requested: "Requested",
  pending: "Pending", running: "Running", started: "Started", completed: "Completed",
  success: "Succeeded", passed: "Passed", failure: "Failed", failed: "Failed",
  cancelled: "Cancelled", canceled: "Cancelled", skipped: "Skipped",
  timed_out: "Timed out", action_required: "Action required", neutral: "Neutral", stale: "Stale",
};

export function ciStateText(value: string) {
  return STATE_LABELS[value] ?? humanize(value);
}

export function ciJobStateText(job: CIJob) {
  return job.status === "completed" && job.conclusion ? ciStateText(job.conclusion) : ciStateText(job.status);
}

export function ciJobPhase(job: CIJob) {
  if (job.status === "completed") return job.conclusion === "success" || job.conclusion === "neutral" || job.conclusion === "skipped" ? "succeeded" : "failed";
  return ["queued", "waiting", "requested", "pending"].includes(job.status) ? "drafted" : "running";
}

export function ciJobActive(job: CIJob) {
  return ["queued", "in_progress", "waiting", "requested", "pending", "running", "started"].includes(job.status) && !job.conclusion;
}

export function filterCIFJobs(jobs: CIJob[], query: string) {
  const needle = query.trim().toLowerCase();
  if (!needle) return jobs;
  return jobs.filter(job => [job.repository, job.name, job.workflowName, job.ref].some(field => field.toLowerCase().includes(needle)));
}

export function formatDurationSeconds(totalSeconds: number) {
  if (!Number.isFinite(totalSeconds) || totalSeconds < 0) return "0s";
  const seconds = Math.floor(totalSeconds);
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const rest = seconds % 60;
  if (hours) return minutes ? `${hours}h ${minutes}m` : `${hours}h`;
  if (minutes) return rest ? `${minutes}m ${rest}s` : `${minutes}m`;
  return `${rest}s`;
}

export function formatDurationMs(milliseconds: number) {
  if (!Number.isFinite(milliseconds) || milliseconds < 0) return "0ms";
  if (milliseconds < 1000) return `${Math.round(milliseconds)}ms`;
  return formatDurationSeconds(Math.round(milliseconds / 1000));
}

export function formatReportedDurationMs(value: number | null | undefined) {
  return value == null ? "Duration unavailable" : formatDurationMs(value);
}

export function formatMoneyMicros(micros: number, currency: string) {
  const amount = Math.abs(micros) / 1_000_000;
  const text = (micros < 0 ? "-" : "") + amount.toFixed(6).replace(/0+$/, "").replace(/\.$/, "");
  return currency === "USD" ? `$${text}` : `${currency} ${text}`;
}

export function formatRatePerMinute(microsPerMinute: number, currency: string) {
  return `${formatMoneyMicros(microsPerMinute, currency)}/min`;
}

export function freePeriodText(freeSecondsPerRun: number) {
  if (freeSecondsPerRun % 3600 === 0) {
    const hours = freeSecondsPerRun / 3600;
    return `${hours} hour${hours === 1 ? "" : "s"}`;
  }
  if (freeSecondsPerRun % 60 === 0) {
    const minutes = freeSecondsPerRun / 60;
    return `${minutes} minute${minutes === 1 ? "" : "s"}`;
  }
  return `${freeSecondsPerRun} second${freeSecondsPerRun === 1 ? "" : "s"}`;
}

export function monthlyQuotaText(monthlyMinuteLimit: number | null) {
  return monthlyMinuteLimit == null ? "No monthly minute quota." : `Monthly minute limit: ${monthlyMinuteLimit}.`;
}

export function safeGitHubURL(value: string | null | undefined) {
  if (!value) return null;
  let url: URL;
  try { url = new URL(value); } catch { return null; }
  if (url.protocol !== "https:" || url.hostname !== "github.com" || url.username || url.password || url.port) return null;
  return url.href;
}

export function runsOnLabel(size: CIPricingSize) {
  return size.label;
}

export function ciSizeLabel(sizes: CIPricingSize[], size: string) {
  return sizes.find(item => item.id === size || item.label === size)?.label ?? size;
}

export function sizeSummary(size: CIPricingSize) {
  const memory = size.memoryMiB >= 1024 ? `${(size.memoryMiB / 1024).toFixed(0)} GiB` : `${size.memoryMiB} MiB`;
  return `${size.vcpus} vCPU · ${memory}`;
}

export function poolStateText(pool: CIPool) {
  return pool.ready ? "Ready" : ciStateText(pool.status) || "Unavailable";
}

export function featureStateText(status: string) {
  return { available: "Available", preview: "Preview", not_configured: "Not configured" }[status] ?? humanize(status);
}

export function metricText(value: number | null | undefined, format: (value: number) => string) {
  return value == null ? "unavailable" : format(value);
}

export function parseSpendCapCents(text: string) {
  const trimmed = text.trim();
  if (!/^\d+(\.\d{1,2})?$/.test(trimmed)) return null;
  const cents = Math.round(Number(trimmed) * 100);
  return Number.isSafeInteger(cents) && cents >= 1 && cents <= 100000 ? cents : null;
}

export function spendCapText(cents: number) {
  return (cents / 100).toFixed(2);
}

export function parseCount(text: string) {
  const trimmed = text.trim();
  if (!/^\d+$/.test(trimmed)) return null;
  const value = Number(trimmed);
  return Number.isSafeInteger(value) ? value : null;
}

export function shortSha(sha: string) {
  return sha.length > 7 ? `${sha.slice(0, 7)}…` : sha;
}

function ciDiagTimestamp(value: string | null | undefined) {
  if (!value) return "Unavailable";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "Unavailable" : date.toISOString();
}

export function ciRunnerEvidenceRows(d: CIRunnerDiagnostics | null | undefined): { label: string; value: string }[] {
  if (!d) return [];
  const association = d.association === "assigned" ? "Assigned runner" : d.association === "provisioning_attempt" ? "Provisioning attempt" : "Unknown";
  const settled = d.createSettled == null ? "Unavailable" : d.createSettled ? "Settled" : "Not settled";
  return [
    { label: "Runner", value: d.runnerId },
    { label: "Association", value: association },
    { label: "Phase", value: ciStateText(d.phase) },
    { label: "Created", value: ciDiagTimestamp(d.createdAt) },
    { label: "Updated", value: ciDiagTimestamp(d.updatedAt) },
    { label: "Creation attempted", value: ciDiagTimestamp(d.createAttemptedAt) },
    { label: "Creation settled", value: settled },
    { label: "Delete request", value: d.deleteRequested ? "Requested" : "Not requested" },
    { label: "Setup deadline", value: ciDiagTimestamp(d.setupDeadlineAt) },
    { label: "Hard deadline", value: ciDiagTimestamp(d.hardDeadlineAt) },
    { label: "Execution started", value: ciDiagTimestamp(d.executionStartedAt) },
    { label: "Execution stop (settled)", value: ciDiagTimestamp(d.executionStoppedAt) },
    { label: "Execution deadline", value: ciDiagTimestamp(d.deadlineAt) },
    { label: "Stop requested", value: ciDiagTimestamp(d.stopRequestedAt) },
    { label: "Stop reason", value: d.stopReason ? ciStateText(d.stopReason) : "Not recorded" },
    { label: "Billing cutoff", value: ciDiagTimestamp(d.billingStopAt) },
    { label: "Stop verified", value: ciDiagTimestamp(d.stoppedVerifiedAt) },
    { label: "Cleanup verified", value: ciDiagTimestamp(d.cleanupVerifiedAt) },
  ];
}

// ciLogCollectionNotice renders only the server's collection state. It never
// derives job success, elapsed time, or log counts from that state.
export function ciLogCollectionNotice(collection?: CILogCollection | null): string | null {
  if (!collection) return null;
  switch (collection.state) {
    case "collecting":
      return "Fetching completed job logs from GitHub.";
    case "unavailable":
      return "GitHub job logs are unavailable; collection will retry while this job remains eligible.";
    case "complete": {
      const at = collection.collectedAt;
      if (typeof at !== "string" || at === "" || Number.isNaN(new Date(at).getTime())) {
        return "GitHub log collection proof is unavailable.";
      }
      const notice = "GitHub Actions logs collected after job completion.";
      return collection.truncated ? `${notice} Output is limited to the first 1 MiB.` : notice;
    }
    default:
      return "GitHub log collection status is unavailable.";
  }
}
