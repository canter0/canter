"use client";
import { useCallback, useEffect, useState } from "react";
import { canterFetch, CanterAPIError } from "./canter-api";
import type { Wren } from "./wrens";
import { coalescedResourceRefresh, workUpdateMatchesPath } from "./wren-resource-refresh";

export type WrenJobRecord = { canSteer?: boolean; steering?: {id:string;content:string;createdAt:string;appliedAt?:string}[]; id: string; workspaceId: string; wrenId: string; lead: Wren; originConversationId: string; parentRunId: string; title: string; prompt: string; status: string; result?: string; failure?: string; executorStarted: boolean; cancelledAt?: string; publishedAt?: string; returnRunId?: string; returnSuppressed: boolean; createdAt: string; updatedAt: string };
export type WrenComputerCheckpointRecord = {id:string;conversationId:string;computerId:string;scope:"private"|"room";state:"creating"|"ready"|"restoring"|"deleting"|"failed";includeBrowser:boolean;sleepAfterSaving:boolean;createdAt:string;updatedAt:string;rawBytes:number;compressedBytes:number;fileCount:number;failure?:string;ramSupported:boolean;processResumeSupported:boolean;browserLoginGuaranteed:boolean};
export type WrenComputerRecord = { restoreCheckpointId?:string;unsettledCommands?:number;unsettledCodingWork?:number;busyReason?:string; id: string; workspaceId: string; conversationId: string; ownerId: string; scopeKind: "private" | "room"; name: string; phase: string; vcpus: number; memoryMiB: number; diskGiB: number; monthlyCents: number; estimatedCents: number; failure?: string; deleteRequested: boolean; expiresAt: string; createdAt: string; updatedAt: string; lastUsedAt?: string; suspendSupported: boolean; controlAccountId?: string; controlName?: string; controlExpiresAt?: string };
export type WrenComputerList = { computers: WrenComputerRecord[]; enabled: boolean; suspendSupported: boolean };
export type WrenArtifactRecord = { id: string; workspaceId: string; conversationId: string; revision: number; latestRevision: number; name: string; mediaType: string; size: number; sha256: string; authorId: string; provenance: { runId?: string; jobId?: string; computerId?: string; path?: string; sources?: string[]; summary?: string }; createdAt: string; updatedAt: string };
export type WrenFileEntry = { name: string; directory: boolean; size: number; symlink?: boolean };
export type WrenFileChunk = { path: string; data: string; size: number; offset: number; nextOffset: number; eof: boolean };
export type WrenDesktopInput = { kind: "drag" | "double_click" | "click" | "move" | "scroll" | "type" | "key" | "url"; x?: number; y?: number; x2?: number; y2?: number; button?: number; amount?: number; text?: string; key?: string; url?: string };
export const workBase = (workspace: string) => `/workspaces/${encodeURIComponent(workspace)}`;
export const workUpdateEvent = "canter-wren-work-updated";
export function notifyWorkUpdate(workspaceId: string, conversationId?: string) { window.dispatchEvent(new CustomEvent(workUpdateEvent, { detail: { workspaceId, conversationId } })); }
export function workError(cause: unknown, fallback: string) { return cause instanceof Error ? cause.message : fallback; }
export { activeJob, jobLabel, computerLabel, fileSize, displayMoney, desktopPoint, compareWorkTimestamps } from "./wren-work-state";
export function useWrenResource<T>(path: string, pollMs?: number, active?: (value: T) => boolean) {
  const [revision, setRevision] = useState(0);
  const [state, setState] = useState<{ path: string; data?: T; error: string }>({ path: "", error: "" });
  const refresh = useCallback(() => setRevision(value => value + 1), []);
  useEffect(() => {
    if (!path) return;
    const controller = new AbortController();
    let shouldPoll = true, failures = 0, nextRetryAt = 0;
    async function load(isCurrent: () => boolean) {
      if (controller.signal.aborted) return;
      try { const data = await canterFetch<T>(path, { signal: controller.signal }); if (!isCurrent()) return; failures = 0; nextRetryAt = 0; shouldPoll = active ? active(data) : false; setState({ path, data, error: "" }); }
      catch (cause) { if (!isCurrent()) return; failures++; nextRetryAt = Date.now() + Math.min(30000, 1000 * 2 ** Math.min(failures,5)); if (cause instanceof CanterAPIError && [401,403,404].includes(cause.status)) shouldPoll = false; if (!controller.signal.aborted) setState(previous => ({ path, data: cause instanceof CanterAPIError && [401,403,404].includes(cause.status) ? undefined : previous.path === path ? previous.data : undefined, error: workError(cause, "This work could not be loaded.") })); }
    }
    const loader = coalescedResourceRefresh(load);
    loader.request();
    const visible = () => { if (document.visibilityState === "visible") loader.request(); };
    const updated = (event: Event) => { if (event instanceof CustomEvent && workUpdateMatchesPath(path, event.detail)) loader.request(); };
    const timer = pollMs ? window.setInterval(() => { if (shouldPoll && Date.now() >= nextRetryAt) visible(); }, pollMs) : undefined;
    window.addEventListener("focus", visible); document.addEventListener("visibilitychange", visible); window.addEventListener(workUpdateEvent, updated);
    return () => { loader.dispose(); controller.abort(); if (timer) window.clearInterval(timer); window.removeEventListener("focus", visible); document.removeEventListener("visibilitychange", visible); window.removeEventListener(workUpdateEvent, updated); };
  }, [path, pollMs, active, revision]);
  return { data: state.path === path ? state.data : undefined, error: state.path === path ? state.error : "", loading: !!path && (state.path !== path || !state.data && !state.error), refresh };
}
export async function fetchWrenBlob(path: string, signal?: AbortSignal) {
  const response = await fetch(`/api/canter${path}`, { credentials: "include", cache: "no-store", signal });
  if (!response.ok) { let message = response.statusText; try { const body = await response.json(); message = typeof body.error === "string" ? body.error : body.error?.message ?? body.message ?? message; } catch { /* Use the HTTP reason. */ } throw new CanterAPIError(response.status, message); }
  return response.blob();
}
export function downloadWrenBlob(blob: Blob, name: string) {
  const url = URL.createObjectURL(blob), link = document.createElement("a");
  link.href = url; link.download = name.replace(/[\\/\x00-\x1f]/g, "_"); link.click(); window.setTimeout(() => URL.revokeObjectURL(url), 30000);
}
