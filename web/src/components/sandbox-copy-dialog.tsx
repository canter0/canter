"use client";

import { useRouter } from "next/navigation";
import { useCallback, useRef, useState, type FormEvent } from "react";
import { useDialog } from "@/components/use-dialog";
import dialogStyles from "@/components/agent-connection-dialog.module.css";
import styles from "@/components/workspace.module.css";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { canterFetch } from "@/lib/canter-api";
import { useRequestScope } from "@/components/use-request-scope";
import { compatibleSandboxSizes, formatMiB, sandboxDesktopEligible, sandboxHourlyCents, type Sandbox, type SandboxCreateSource, type SandboxSize } from "@/lib/sandboxes";

// SandboxCopyDialog creates a separate sandbox that starts from saved files.
// The captured source never changes while the dialog is open; the submitted
// body and idempotency key are held across retries so an uncertain response
// cannot create a second machine.
export function SandboxCopyDialog({ workspaceId, source, sizes, onClose }: {
	workspaceId: string;
	source: SandboxCreateSource;
	sizes: SandboxSize[];
	onClose: () => void;
}) {
	const { signal } = useRequestScope();
	const router = useRouter();
	const ref = useRef<HTMLDialogElement>(null);
	const dismiss = useDialog(ref, "input");
	const compatible = compatibleSandboxSizes(sizes, source.diskGiB);
	const [name, setName] = useState("");
	const preferred = compatible.find(size => size.id === source.sourceSize) ?? compatible[0];
	const [size, setSize] = useState(preferred?.id ?? "");
	const [desktop, setDesktop] = useState(false);
	const [key, setKey] = useState(() => crypto.randomUUID());
	const [submitted, setSubmitted] = useState<string | null>(null);
	const [error, setError] = useState("");
	const [pending, setPending] = useState(false);
	const pendingRef = useRef(false);
	const locked = submitted !== null;

	const close = useCallback(() => { if (!pendingRef.current) dismiss(onClose); }, [dismiss, onClose]);

	async function submit(event: FormEvent) {
		event.preventDefault();
		if (pendingRef.current) return;
		const requestSignal = signal();
		if (requestSignal.aborted) return;
		pendingRef.current = true;
		setPending(true);
		setError("");
		try {
			const body = submitted ?? JSON.stringify({
				name: name.trim(), size, desktop,
				...(source.templateId ? { templateId: source.templateId, templateVersion: source.templateVersion } : { snapshotId: source.snapshotId }),
				idempotencyKey: key,
			});
			setSubmitted(body);
			const created = await canterFetch<Sandbox>(`/workspaces/${encodeURIComponent(workspaceId)}/sandboxes`, { method: "POST", body, signal: requestSignal });
			if (!requestSignal.aborted) router.push(`/app/sandboxes/${encodeURIComponent(created.id)}`);
		} catch (cause) {
			if (!requestSignal.aborted) setError(cause instanceof Error ? cause.message : "Couldn't create the sandbox.");
		} finally {
			pendingRef.current = false;
			if (!requestSignal.aborted) setPending(false);
		}
	}

	return <dialog ref={ref} className={dialogStyles.dialog} aria-labelledby="copy-sandbox-title" onCancel={event => { event.preventDefault(); close(); }}>
		<button className={dialogStyles.close} aria-label="Close" disabled={pending} onClick={close}><WorkspaceIcon name="close" width="17" height="17" /></button>
		<h2 id="copy-sandbox-title">New sandbox</h2>
		<p>From {source.label}. A separate sandbox starts from these saved files.</p>
		<form onSubmit={submit}>
			<label className={dialogStyles.field}>Name<input type="text" value={name} onChange={event => setName(event.target.value)} maxLength={80} required disabled={pending || locked} /></label>
			<label className={dialogStyles.field}>Size<select value={size} onChange={event => {
				const next = event.target.value;
				setSize(next);
				if (!sandboxDesktopEligible(sizes.find(entry => entry.id === next))) setDesktop(false);
			}} disabled={pending || locked}>
				{compatible.map(option => <option key={option.id} value={option.id}>{option.id} — {option.vcpus} vCPU, {formatMiB(option.memoryMiB)} RAM, {option.diskGiB} GiB disk{option.free ? " (free tier)" : ` (${sandboxHourlyCents(option.monthlyCents)})`}</option>)}
			</select></label>
			<label className={dialogStyles.field} style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
				<input type="checkbox" checked={desktop} onChange={event => setDesktop(event.target.checked)} disabled={pending || locked || !sandboxDesktopEligible(sizes.find(entry => entry.id === size))} />
				<span>Desktop</span>
			</label>
			<p className={dialogStyles.field} style={{ marginTop: -8 }}>A shared graphical desktop. Requires at least 2 vCPUs and 4 GiB RAM.</p>
			<p>Free sandboxes sleep after 15 idle minutes and share 50 awake-hours monthly; paid sizes bill while running.</p>
			{error ? <p className={dialogStyles.error} role="alert">{error}</p> : null}
			{locked && !pending ? <p>Retry sends the same request. Check your sandbox list before starting a separate request. <button type="button" className={styles.secondaryButton} onClick={() => {
				setSubmitted(null);
				setKey(crypto.randomUUID());
				setError("");
			}}>Edit as new request</button></p> : null}
			<button className={dialogStyles.primary} type="submit" disabled={pending || !compatible.length}>{pending ? "Creating…" : locked ? "Retry request" : "Create sandbox"}</button>
		</form>
	</dialog>;
}
