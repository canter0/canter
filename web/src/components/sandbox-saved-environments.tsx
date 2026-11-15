"use client";

import Link from "next/link";
import { useCallback, useEffect, useRef, useState } from "react";
import { useDialog } from "@/components/use-dialog";
import { useRequestScope } from "@/components/use-request-scope";
import dialogStyles from "@/components/agent-connection-dialog.module.css";
import styles from "@/components/workspace.module.css";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { canterFetch, relativeTime } from "@/lib/canter-api";
import {
	canCopySandboxSnapshot,
	compatibleSandboxSizes,
	formatBytes,
	type Sandbox,
	type SandboxCreateSource,
	type SandboxListResponse,
	type SandboxSnapshot,
	type SandboxSnapshotPage,
	type SandboxTemplate,
	type SandboxTemplateList,
} from "@/lib/sandboxes";
import { SandboxCopyDialog } from "@/components/sandbox-copy-dialog";

type Dialog =
	| { kind: "rename"; snapshot: SandboxSnapshot }
	| { kind: "template"; snapshot: SandboxSnapshot }
	| { kind: "copy"; source: SandboxCreateSource };

// SandboxSavedEnvironments is the organization library of saved environments.
// The server is authoritative: reads never expose keys/manifests/options, and
// every mutation re-fetches rather than patching local state.
export function SandboxSavedEnvironments({ workspaceId, writer, sandbox, onRestored }: {
	workspaceId: string;
	writer: boolean;
	sandbox?: Sandbox;
	onRestored?: (sandbox: Sandbox) => void;
}) {
	const { signal: requestSignal } = useRequestScope();
	const [snapshots, setSnapshots] = useState<SandboxSnapshot[]>([]);
	const [nextCursor, setNextCursor] = useState("");
	const [historyEnabled, setHistoryEnabled] = useState(false);
	const [templates, setTemplates] = useState<SandboxTemplate[]>([]);
	const [list, setList] = useState<SandboxListResponse | null>(null);
	const [reading, setReading] = useState(false);
	const [error, setError] = useState("");
	const [notice, setNotice] = useState("");
	const [busy, setBusy] = useState("");
	const [dialog, setDialog] = useState<Dialog | null>(null);
	const [expanded, setExpanded] = useState<Record<string, SandboxTemplate>>({});
	const loadSequence = useRef(0);
	const detailSequence = useRef<Record<string, number>>({});
	const expandedIDs = useRef(new Set<string>());
	const readLock = useRef(false);
	const mutationLock = useRef(false);
	const base = `/workspaces/${encodeURIComponent(workspaceId)}`;
	const loaded = list !== null;
	const enabled = list?.enabled === true;
	const sizes = list?.sizes ?? [];
	const machines = list?.sandboxes ?? [];

	const loadTemplate = useCallback(async (id: string): Promise<boolean> => {
		const signal = requestSignal();
		if (signal.aborted || !expandedIDs.current.has(id)) return false;
		detailSequence.current[id] = (detailSequence.current[id] ?? 0) + 1;
		const sequence = detailSequence.current[id];
		try {
			const detail = await canterFetch<SandboxTemplate>(`${base}/sandbox-templates/${encodeURIComponent(id)}`, { signal });
			if (signal.aborted || sequence !== detailSequence.current[id] || !expandedIDs.current.has(id)) return false;
			setExpanded(current => ({ ...current, [id]: detail }));
			return true;
		} catch (cause) {
			if (!signal.aborted && sequence === detailSequence.current[id] && expandedIDs.current.has(id)) {
				setError(cause instanceof Error ? cause.message : "Couldn't load that template.");
			}
			return false;
		}
	}, [base, requestSignal]);

	const closeExpanded = useCallback((id: string) => {
		expandedIDs.current.delete(id);
		detailSequence.current[id] = (detailSequence.current[id] ?? 0) + 1;
		setExpanded(current => {
			const next = { ...current };
			delete next[id];
			return next;
		});
	}, []);

	// The read lock stops same-tick Refresh/Load-more overlap; mutations hold a
	// separate lock so a mutation's own refresh can still run.
	const load = useCallback(async (mode: "initial" | "refresh" | "more"): Promise<boolean> => {
		if (readLock.current) return false;
		const signal = requestSignal();
		if (signal.aborted) return false;
		readLock.current = true;
		setReading(true);
		const sequence = ++loadSequence.current;
		try {
			if (mode === "more") {
				const page = await canterFetch<SandboxSnapshotPage>(`${base}/sandbox-snapshots?before=${encodeURIComponent(cursor.current)}`, { signal });
				if (signal.aborted || sequence !== loadSequence.current) return false;
				setSnapshots(current => {
					const seen = new Set(current.map(entry => entry.id));
					return [...current, ...page.snapshots.filter(entry => !seen.has(entry.id))];
				});
				cursor.current = page.nextCursor ?? "";
				setNextCursor(cursor.current);
				setHistoryEnabled(page.historyEnabled);
				setError("");
				return true;
			}
			const [page, templateList, sandboxes] = await Promise.all([
				canterFetch<SandboxSnapshotPage>(`${base}/sandbox-snapshots`, { signal }),
				canterFetch<SandboxTemplateList>(`${base}/sandbox-templates`, { signal }),
				canterFetch<SandboxListResponse>(`${base}/sandboxes`, { signal }),
			]);
			if (signal.aborted || sequence !== loadSequence.current) return false;
			setSnapshots(page.snapshots);
			cursor.current = page.nextCursor ?? "";
			setNextCursor(cursor.current);
			setTemplates(templateList.templates);
			setHistoryEnabled(templateList.historyEnabled);
			setList(sandboxes);
			// Drop expanded details for templates that no longer exist; refetch
			// the rest so opened views track the authoritative list.
			const live = new Set(templateList.templates.map(template => template.id));
			const stale = [...expandedIDs.current].filter(id => !live.has(id));
			for (const id of stale) closeExpanded(id);
			const remaining = [...expandedIDs.current];
			const results = await Promise.all(remaining.map(id => loadTemplate(id)));
			if (signal.aborted || sequence !== loadSequence.current) return false;
			const complete = results.every(Boolean);
			if (complete) setError("");
			else setError("Some template versions couldn't refresh. Try again before using them.");
			return complete;
		} catch (cause) {
			if (signal.aborted || sequence !== loadSequence.current) return false;
			setError(cause instanceof Error ? cause.message : "Couldn't load saved environments.");
			return false;
		} finally {
			readLock.current = false;
			if (!signal.aborted && sequence === loadSequence.current) setReading(false);
		}
	}, [base, closeExpanded, loadTemplate, requestSignal]);
	const cursor = useRef("");
	useEffect(() => { queueMicrotask(() => void load("initial")); }, [load]);

	const refresh = useCallback(() => load("refresh"), [load]);

	// Returns true only when the WRITE succeeded — a failed refresh after a
	// successful write still counts as success (the error is shown separately).
	const mutate = useCallback(async (label: string, work: () => Promise<void>): Promise<boolean> => {
		if (mutationLock.current || readLock.current) return false;
		const signal = requestSignal();
		if (signal.aborted || !writer || !enabled) return false;
		mutationLock.current = true;
		setBusy(label);
		setError("");
		setNotice("");
		try {
			await work();
			if (signal.aborted) return false;
			setNotice(label);
			const refreshed = await refresh();
			if (!signal.aborted && !refreshed) setError("The change applied, but the library couldn't refresh. Use Refresh to load current data.");
			return !signal.aborted;
		} catch (cause) {
			if (!signal.aborted) setError(cause instanceof Error ? cause.message : "That change didn't apply.");
			return false;
		} finally {
			mutationLock.current = false;
			if (!signal.aborted) setBusy("");
		}
	}, [enabled, refresh, requestSignal, writer]);

	const writesOff = !writer || !enabled || !loaded || !!error || busy !== "" || reading;

	function snapshotLabel(snapshot: SandboxSnapshot) { return snapshot.name || "Saved environment"; }
	function copyable(snapshot: SandboxSnapshot) {
		return canCopySandboxSnapshot(snapshot.id, historyEnabled, machines) && compatibleSandboxSizes(sizes, snapshot.sourceDiskGiB).length > 0;
	}
	function snapshotSource(snapshot: SandboxSnapshot): SandboxCreateSource {
		return { label: snapshotLabel(snapshot), diskGiB: snapshot.sourceDiskGiB, sourceSize: snapshot.sourceSize, snapshotId: snapshot.id };
	}
	function restoreAllowed(snapshot: SandboxSnapshot) {
		if (!sandbox) return false;
		if (sandbox.hasCheckpoint && !sandbox.snapshotId) return false;
		if (sandbox.phase !== "asleep" || !(sandbox.generation ?? 0) || !historyEnabled) return false;
		if (sandbox.snapshotId === snapshot.id) return false;
		const destination = sizes.find(size => size.id === sandbox.size);
		return !!destination && destination.diskGiB >= snapshot.sourceDiskGiB;
	}
	function restoreExplanation(snapshot: SandboxSnapshot) {
		if (!sandbox) return "";
		if (sandbox.hasCheckpoint && !sandbox.snapshotId) return "Wake and sleep this sandbox once before selecting another save.";
		if (sandbox.phase !== "asleep") return "Sleep this sandbox first to choose the save it restores next.";
		if (sandbox.snapshotId === snapshot.id) return "This save already restores on next wake.";
		return "";
	}

	const rename = useCallback(async (snapshot: SandboxSnapshot, name: string) => {
		return mutate(`Renamed ${snapshotLabel(snapshot)}.`, async () => {
			const signal = requestSignal();
			await canterFetch(`${base}/sandbox-snapshots/${encodeURIComponent(snapshot.id)}`, { method: "PATCH", body: JSON.stringify({ name: name.trim() }), signal });
		});
	}, [base, mutate, requestSignal]);
	const keep = useCallback(async (snapshot: SandboxSnapshot) => {
		if (snapshot.retained && !window.confirm(`Stop keeping ${snapshotLabel(snapshot)}? It may be removed once no sandbox or template uses it.`)) return false;
		return mutate(snapshot.retained ? `Stopped keeping ${snapshotLabel(snapshot)}.` : `Keeping ${snapshotLabel(snapshot)}.`, async () => {
			const signal = requestSignal();
			await canterFetch(`${base}/sandbox-snapshots/${encodeURIComponent(snapshot.id)}`, { method: "PATCH", body: JSON.stringify({ retained: !snapshot.retained }), signal });
		});
	}, [base, mutate, requestSignal]);
	const saveAsTemplate = useCallback(async (snapshot: SandboxSnapshot, templateId: string, name: string) => {
		return mutate(`Saved ${snapshotLabel(snapshot)} as a template.`, async () => {
			const signal = requestSignal();
			await canterFetch(templateId ? `${base}/sandbox-templates/${encodeURIComponent(templateId)}/versions` : `${base}/sandbox-templates`, {
				method: "POST",
				body: JSON.stringify(templateId ? { snapshotId: snapshot.id } : { name: name.trim(), snapshotId: snapshot.id }),
				signal,
			});
		});
	}, [base, mutate, requestSignal]);
	const restore = useCallback(async (snapshot: SandboxSnapshot) => {
		if (!sandbox) return;
		if (!window.confirm(`Use this save when ${sandbox.name} next wakes? Your current save will be kept as paid history. This does not wake the sandbox.`)) return;
		return mutate(`Selected ${snapshotLabel(snapshot)} for the next wake.`, async () => {
			const signal = requestSignal();
			const updated = await canterFetch<Sandbox>(`${base}/sandboxes/${encodeURIComponent(sandbox.id)}/restore`, {
				method: "POST",
				body: JSON.stringify({ snapshotId: snapshot.id, expectedGeneration: sandbox.generation }),
				signal,
			});
			if (!signal.aborted) onRestored?.(updated);
		});
	}, [base, mutate, onRestored, requestSignal, sandbox]);
	const deleteTemplate = useCallback(async (template: SandboxTemplate) => {
		if (!window.confirm(`Delete ${template.name}? Existing sandboxes keep their saves.`)) return;
		return mutate(`Deleted ${template.name}.`, async () => {
			const signal = requestSignal();
			await canterFetch(`${base}/sandbox-templates/${encodeURIComponent(template.id)}`, { method: "DELETE", signal });
			if (!signal.aborted) closeExpanded(template.id);
		});
	}, [base, closeExpanded, mutate, requestSignal]);
	const deleteVersion = useCallback(async (template: SandboxTemplate, version: number) => {
		if (!window.confirm(`Delete version ${version} of ${template.name}? Existing sandboxes keep their saves.`)) return;
		return mutate(`Deleted version ${version} of ${template.name}.`, async () => {
			const signal = requestSignal();
			await canterFetch(`${base}/sandbox-templates/${encodeURIComponent(template.id)}/versions/${version}`, { method: "DELETE", signal });
		});
	}, [base, mutate, requestSignal]);

	return <section style={{ marginTop: 28 }}>
		<div className={styles.pageHeading} style={{ marginBottom: 16 }}>
			<div><h2>Saved environments</h2>
				<p>A free sandbox’s current save is included. Extra kept saves and templates require billing and use storage pricing. Shared saves are stored once.</p></div>
			<div className={styles.pageActions}>
				<button className={styles.secondaryButton} onClick={() => void load("refresh")} disabled={!loaded || reading || busy !== ""}>Refresh</button>
			</div>
		</div>
		<p className={styles.status}>Saves keep files and supported package setup — not running processes or browser logins.{!historyEnabled && loaded ? <> Extra saves and templates need <Link href="/app/billing">billing</Link>.</> : null}</p>
		{notice ? <p role="status" className={styles.loading}>{notice}</p> : null}
		{error ? <div role="alert" className={styles.error}>{error} <button onClick={() => void load("refresh")} disabled={reading || busy !== ""}>Try again</button></div> : null}
		{!loaded && !error ? <p role="status" className={styles.loading}>Loading saved environments…</p> : null}
		{loaded && !enabled ? <p className={styles.status}>Enable Sandboxes in <Link href="/app/products">Products</Link> to manage saved environments.</p> : null}
		{loaded && enabled ? <>
			<h3>Saves</h3>
			{snapshots.length === 0 ? <p className={styles.status}>Sleep a sandbox to save its environment.</p> : <div className={styles.cardGrid}>{snapshots.map(snapshot => <div key={snapshot.id} className={styles.resourceCard} data-motion-card>
				<div className={styles.resourceCardHeader}><div><WorkspaceIcon name="folder" /><h2>{snapshotLabel(snapshot)}</h2></div>
					<span className={styles.status} data-phase="succeeded">{snapshot.readyAt ? relativeTime(snapshot.readyAt) : relativeTime(snapshot.createdAt)}</span></div>
				<p>{formatBytes(snapshot.storedBytes)} · {snapshot.sourceSize} · needs at least {snapshot.sourceDiskGiB} GiB disk</p>
				<p className={styles.status}>
					{snapshot.retained ? "Kept" : "Not kept"}
					{sandbox?.snapshotId === snapshot.id ? " · Current save" : ""}
					{machines.some(machine => machine.snapshotId === snapshot.id && machine.tier === "free" && !["deleting", "deleted"].includes(machine.phase)) ? " · free current save" : ""}
				</p>
				{writer ? <div className={styles.resourceCardFooter} style={{ gap: 8, flexWrap: "wrap", justifyContent: "flex-start" }}>
					<button className={styles.secondaryButton} disabled={writesOff} onClick={() => setDialog({ kind: "rename", snapshot })}>Rename</button>
					<button className={styles.secondaryButton} disabled={writesOff || (!snapshot.retained && !historyEnabled)} title={!snapshot.retained && !historyEnabled ? "Keeping extra saves requires billing" : ""} onClick={() => void keep(snapshot)}>{snapshot.retained ? "Stop keeping" : "Keep save"}</button>
					<button className={styles.secondaryButton} disabled={writesOff || !historyEnabled} title={historyEnabled ? "" : "Extra saves and templates require billing"} onClick={() => setDialog({ kind: "template", snapshot })}>Save as template</button>
					<button className={styles.secondaryButton} disabled={writesOff || !copyable(snapshot)} title={copyable(snapshot) ? "" : "Requires billing or a compatible size"} onClick={() => setDialog({ kind: "copy", source: snapshotSource(snapshot) })}>Create sandbox</button>
					{sandbox ? <button className={styles.primaryButton} disabled={writesOff || !restoreAllowed(snapshot)} onClick={() => void restore(snapshot)}>Use on next wake</button> : null}
				</div> : null}
				{sandbox ? <p className={styles.status}>{restoreExplanation(snapshot)}</p> : null}
			</div>)}</div>}
			{nextCursor ? <button className={styles.secondaryButton} disabled={reading || busy !== ""} onClick={() => void load("more")}>Load more</button> : null}

			<h3 style={{ marginTop: 24 }}>Templates</h3>
			{templates.length === 0 ? <p className={styles.status}>No templates yet. Save an environment as a template to reuse it across sandboxes.</p> : <div className={styles.cardGrid}>{templates.map(template => {
				const detail = expanded[template.id];
				const latest = template.latest;
				return <div key={template.id} className={styles.resourceCard} data-motion-card>
					<div className={styles.resourceCardHeader}><div><WorkspaceIcon name="folder" /><h2>{template.name}</h2></div>
						{latest ? <span className={styles.status}>v{latest.version} · {latest.sourceSize}</span> : <span className={styles.status}>no versions</span>}</div>
					{latest ? <p>{formatBytes(latest.storedBytes)} · needs at least {latest.sourceDiskGiB} GiB disk</p> : <p className={styles.status}>This template has no saved versions.</p>}
					<div className={styles.resourceCardFooter} style={{ gap: 8, flexWrap: "wrap", justifyContent: "flex-start" }}>
						<button className={styles.secondaryButton} disabled={reading || busy !== ""} onClick={() => {
							if (detail) { closeExpanded(template.id); return; }
							expandedIDs.current.add(template.id);
							void loadTemplate(template.id);
						}}>{detail ? "Hide versions" : "Versions"}</button>
						{writer && latest && historyEnabled && compatibleSandboxSizes(sizes, latest.sourceDiskGiB).length > 0 ? <button className={styles.secondaryButton} disabled={writesOff} onClick={() => setDialog({ kind: "copy", source: { label: `${template.name} v${latest.version}`, diskGiB: latest.sourceDiskGiB, sourceSize: latest.sourceSize, templateId: template.id, templateVersion: latest.version } })}>Create sandbox</button> : null}
						{writer ? <button className={styles.secondaryButton} disabled={writesOff} onClick={() => void deleteTemplate(template)}>Delete template</button> : null}
					</div>
					{detail ? <ul style={{ listStyle: "none", padding: 0, margin: "8px 0 0" }}>
						{(detail.versions ?? []).map(version => <li key={version.version} style={{ display: "flex", gap: 8, alignItems: "center", padding: "6px 0", flexWrap: "wrap" }}>
							<span className={styles.status}>v{version.version} · {relativeTime(version.createdAt)} · {version.sourceDiskGiB} GiB · {formatBytes(version.storedBytes)}</span>
							{writer ? <button className={styles.secondaryButton} disabled={writesOff || !historyEnabled || compatibleSandboxSizes(sizes, version.sourceDiskGiB).length === 0} onClick={() => setDialog({ kind: "copy", source: { label: `${template.name} v${version.version}`, diskGiB: version.sourceDiskGiB, sourceSize: version.sourceSize, templateId: template.id, templateVersion: version.version } })}>Create sandbox</button> : null}
							{writer ? <button className={styles.secondaryButton} disabled={writesOff} onClick={() => void deleteVersion(detail, version.version)}>Delete</button> : null}
						</li>)}
						{(detail.versions ?? []).length === 0 ? <li className={styles.status}>No saved versions.</li> : null}
					</ul> : null}
				</div>;
			})}</div>}
		</> : null}
		{dialog?.kind === "rename" && writer && enabled ? <RenameDialog snapshot={dialog.snapshot} pending={busy !== ""} error={error} onSubmit={async name => { const signal = requestSignal(); if (await rename(dialog.snapshot, name) && !signal.aborted) setDialog(null); }} onClose={() => setDialog(null)} /> : null}
		{dialog?.kind === "template" && writer && enabled ? <TemplateDialog snapshot={dialog.snapshot} templates={templates} pending={busy !== ""} error={error} onSubmit={async (templateId, name) => { const signal = requestSignal(); if (await saveAsTemplate(dialog.snapshot, templateId, name) && !signal.aborted) setDialog(null); }} onClose={() => setDialog(null)} /> : null}
		{dialog?.kind === "copy" && writer && enabled ? <SandboxCopyDialog workspaceId={workspaceId} source={dialog.source} sizes={sizes} onClose={() => setDialog(null)} /> : null}
	</section>;
}

function RenameDialog({ snapshot, pending, error, onSubmit, onClose }: { snapshot: SandboxSnapshot; pending: boolean; error: string; onSubmit: (name: string) => Promise<void>; onClose: () => void }) {
	const ref = useRef<HTMLDialogElement>(null);
	const dismiss = useDialog(ref, "input");
	const [name, setName] = useState(snapshot.name);
	function close() { if (!pending) dismiss(onClose); }
	return <dialog ref={ref} className={dialogStyles.dialog} aria-labelledby="rename-save-title" onCancel={event => { event.preventDefault(); close(); }}>
		<button className={dialogStyles.close} aria-label="Close" disabled={pending} onClick={close}><WorkspaceIcon name="close" width="17" height="17" /></button>
		<h2 id="rename-save-title">Rename save</h2>
		<form onSubmit={event => { event.preventDefault(); if (!pending && name.trim()) void onSubmit(name); }}>
			<label className={dialogStyles.field}>Name<input type="text" value={name} onChange={event => setName(event.target.value)} maxLength={80} required disabled={pending} /></label>
			{error ? <p className={dialogStyles.error} role="alert">{error}</p> : null}
			<button className={dialogStyles.primary} type="submit" disabled={pending || !name.trim()}>{pending ? "Renaming…" : "Rename"}</button>
		</form>
	</dialog>;
}

function TemplateDialog({ snapshot, templates, pending, error, onSubmit, onClose }: { snapshot: SandboxSnapshot; templates: SandboxTemplate[]; pending: boolean; error: string; onSubmit: (templateId: string, name: string) => Promise<void>; onClose: () => void }) {
	const ref = useRef<HTMLDialogElement>(null);
	const dismiss = useDialog(ref, "input");
	const [templateId, setTemplateId] = useState("");
	const [name, setName] = useState("");
	function close() { if (!pending) dismiss(onClose); }
	return <dialog ref={ref} className={dialogStyles.dialog} aria-labelledby="save-template-title" onCancel={event => { event.preventDefault(); close(); }}>
		<button className={dialogStyles.close} aria-label="Close" disabled={pending} onClick={close}><WorkspaceIcon name="close" width="17" height="17" /></button>
		<h2 id="save-template-title">Save as template</h2>
		<p>{snapshot.name || "Saved environment"} becomes a reusable template version.</p>
		<form onSubmit={event => { event.preventDefault(); if (!pending && (templateId || name.trim())) void onSubmit(templateId, name); }}>
			<label className={dialogStyles.field}>Template<select value={templateId} onChange={event => setTemplateId(event.target.value)} disabled={pending}>
				<option value="">New template</option>
				{templates.map(template => <option key={template.id} value={template.id}>{template.name}</option>)}
			</select></label>
			{!templateId ? <label className={dialogStyles.field}>Template name<input type="text" value={name} onChange={event => setName(event.target.value)} maxLength={80} required disabled={pending} /></label> : null}
			{error ? <p className={dialogStyles.error} role="alert">{error}</p> : null}
			<button className={dialogStyles.primary} type="submit" disabled={pending || (!templateId && !name.trim())}>{pending ? "Saving…" : templateId ? "Add version" : "Create template"}</button>
		</form>
	</dialog>;
}
