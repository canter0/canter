export type Sandbox = {
	desktop?: boolean;
  id: string;
  name: string;
  size: string;
  tier: "free" | "paid";
  phase: string;
  generation?: number;
  monthlyCents: number;
  labels?: Record<string, string>;
  idleTimeoutSeconds?: number;
  failure?: string;
  notice?: string;
  hasCheckpoint?: boolean;
  snapshotId?: string;
  createdByKind: string;
  createdByName?: string;
  lastUsedAt?: string;
  createdAt: string;
};
export type SandboxSize = { id: string; vcpus: number; memoryMiB: number; diskGiB: number; free: boolean; monthlyCents: number };
export type SandboxLimits = { maxCommandSeconds: number; maxFileBytes: number; uploadChunkBytes: number; defaultIdleSeconds: number; maxJobLogBytes: number };
export type SandboxListResponse = { sandboxes: Sandbox[]; enabled: boolean; previewsEnabled?: boolean; sizes?: SandboxSize[]; limits?: SandboxLimits; freeUsage: { usedSeconds: number; limitSeconds: number } };
export type SandboxJob = { jobId: string; status: string; exitCode?: number; updatedAt?: string; logTruncated?: boolean; error?: string };
export type JobLogChunk = { jobId: string; stream: string; dataBase64: string; offset: number; nextOffset: number; eof: boolean; status: string; exitCode?: number; logTruncated?: boolean };
export type SandboxMetrics = { sampledAt: string; uptimeSeconds: number; cpu: { count: number; load1: number; load5: number; load15: number }; memory: { totalBytes: number; availableBytes: number; usedBytes: number }; disk: { totalBytes: number; usedBytes: number; availableBytes: number } };
export type FileUpload = { id: string; path: string; size: number; offset: number; sha256?: string; complete: boolean };

export function sandboxHourlyCents(monthlyCents: number) {
  return `$${(monthlyCents / 720 / 100).toFixed(4)}/h`;
}
export function freeHoursText(used: number, limit: number) {
  return `${Math.floor(used / 3600)} of ${Math.floor(limit / 3600)} free hours this month`;
}
export function formatMiB(mib: number) {
  return mib >= 1024 ? `${(mib / 1024).toFixed(0)} GiB` : `${mib} MiB`;
}
export function formatBytes(bytes: number) {
  if (bytes >= 1 << 30) return `${(bytes / (1 << 30)).toFixed(1)} GB`;
  if (bytes >= 1 << 20) return `${(bytes / (1 << 20)).toFixed(1)} MB`;
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${bytes} B`;
}
export function idleChoiceText(seconds: number, defaultSeconds: number) {
  if (seconds === -1) return `Default (${Math.round(defaultSeconds / 60)} min)`;
  if (seconds === 0) return "Never sleep while awake";
  return `${Math.round(seconds / 60)} minutes`;
}

export function sandboxPhaseText(phase: string) {
	return phase === "save_failed" ? "Save failed" : phase;
}

export type SandboxSnapshot = { id: string; workspaceId: string; sourceSandboxId: string; sourceGeneration: number; sourceSize: string; sourceDiskGiB: number; name: string; state: string; retained: boolean; storedBytes: number; createdAt: string; readyAt?: string };
export type SandboxSnapshotPage = { snapshots: SandboxSnapshot[]; nextCursor?: string; historyEnabled: boolean };
export type SandboxTemplateVersion = { version: number; snapshotId: string; sourceSize: string; sourceDiskGiB: number; storedBytes: number; createdAt: string };
export type SandboxTemplate = { id: string; workspaceId: string; name: string; createdAt: string; updatedAt: string; latest?: SandboxTemplateVersion; versions?: SandboxTemplateVersion[] };
export type SandboxTemplateList = { templates: SandboxTemplate[]; historyEnabled: boolean };
export type SandboxCreateSource = { label: string; diskGiB: number; sourceSize: string } & ({ snapshotId: string; templateId?: never; templateVersion?: never } | { snapshotId?: never; templateId: string; templateVersion: number });
export function compatibleSandboxSizes(sizes: SandboxSize[], diskGiB: number) { return sizes.filter(size => size.diskGiB >= diskGiB); }
export function canCopySandboxSnapshot(snapshotId: string, historyEnabled: boolean, sandboxes: Sandbox[]) { return historyEnabled || sandboxes.some(sandbox => sandbox.snapshotId === snapshotId && sandbox.tier === 'free' && !['deleting', 'deleted'].includes(sandbox.phase)); }

export function sandboxDesktopEligible(size: Pick<SandboxSize, "vcpus" | "memoryMiB"> | undefined) {
	return !!size && size.vcpus >= 2 && size.memoryMiB >= 4096;
}

export type SandboxDesktopControl = {
	mode: "human" | "agent";
	generation: number;
	accountId?: string;
	name?: string;
	leaseToken?: string;
	expiresAt?: string;
};
