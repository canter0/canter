"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { WrenDesktop } from "@/components/wren-desktop";
import type { WrenDesktopInput } from "@/lib/wren-work";
import { useRequestScope } from "@/components/use-request-scope";
import styles from "@/components/workspace.module.css";
import { canterFetch } from "@/lib/canter-api";
import type { Sandbox, SandboxDesktopControl } from "@/lib/sandboxes";

// SandboxDesktop shares one desktop across an organization: one member takes
// human control (input + command gating) while everyone watches. Synchronous
// refs (paused/loadFailed) gate queued callbacks immediately — a failed
// renew/input pauses and stays paused until an explicit Resume/Take.
export function SandboxDesktop({ base, sandbox, accountId, writer }: {
	base: string;
	sandbox: Sandbox;
	accountId: string;
	writer: boolean;
}) {
	const { signal: requestSignal } = useRequestScope();
	const [control, setControl] = useState<SandboxDesktopControl | null>(null);
	const [loadError, setLoadError] = useState("");
	const [actionError, setActionError] = useState("");
	const [paused, setPaused] = useState(false);
	const [busy, setBusy] = useState(false);
	const [now, setNow] = useState(() => Date.now());
	const currentControl = useRef<SandboxDesktopControl | null>(null);
	const pausedRef = useRef(false);
	const loadFailedRef = useRef(false);
	const actionLock = useRef(false);
	const readSequence = useRef(0);
	const endpoint = `${base}/desktop`;
	const generation = sandbox.generation ?? 0;

	const pause = useCallback((message: string) => {
		pausedRef.current = true;
		setPaused(true);
		setActionError(message);
	}, []);

	const clearLoad = useCallback(() => {
		loadFailedRef.current = false;
		setLoadError("");
	}, []);

	// An owned lease needs a real future expiry, a token, the holder account,
	// and the current generation — missing/expired means no control.
	const ownLease = useCallback((): boolean => {
		const c = currentControl.current;
		return !!c && c.mode === "human" && c.accountId === accountId && !!c.leaseToken
			&& !!c.expiresAt && new Date(c.expiresAt).getTime() > Date.now()
			&& c.generation > 0 && c.generation === generation;
	}, [accountId, generation]);

	const read = useCallback(async (): Promise<void> => {
		const signal = requestSignal();
		if (signal.aborted) return;
		const sequence = ++readSequence.current;
		try {
			const state = await canterFetch<SandboxDesktopControl>(`${endpoint}/control`, { signal });
			if (signal.aborted || sequence !== readSequence.current) return;
			currentControl.current = state;
			setControl(state);
			clearLoad();
		} catch (cause) {
			if (signal.aborted || sequence !== readSequence.current) return;
			loadFailedRef.current = true;
			const message = cause instanceof Error ? cause.message : "Couldn't load desktop control.";
			setLoadError(message);
			pause(message);
		}
	}, [clearLoad, endpoint, pause, requestSignal]);

	// 1s clock for lease-expiry render.
	useEffect(() => {
		const tick = window.setInterval(() => setNow(Date.now()), 1000);
		return () => window.clearInterval(tick);
	}, []);

	useEffect(() => {
		queueMicrotask(() => void read());
		const interval = window.setInterval(() => {
			if (!actionLock.current) void read();
		}, 5000);
		return () => window.clearInterval(interval);
	}, [read]);

	// act() is the only control mutation path — claim/release/renew all go
	// through it so every mutation fences reads and checks the live scope.
	const act = useCallback(async (mode: "human" | "agent"): Promise<boolean> => {
		if (!writer) return false;
		const signal = requestSignal();
		if (signal.aborted || actionLock.current) return false;
		actionLock.current = true;
		readSequence.current++; // pending reads can't repaint
		setBusy(true);
		setActionError("");
		const prior = currentControl.current;
		try {
			const state = await canterFetch<SandboxDesktopControl>(`${endpoint}/control`, {
				method: "POST",
				body: JSON.stringify({
					mode,
					expectedGeneration: generation,
					...(prior?.mode === "human" && prior.accountId === accountId && prior.leaseToken ? { leaseToken: prior.leaseToken } : {}),
				}),
				signal,
			});
			if (signal.aborted) return false;
			currentControl.current = state;
			setControl(state);
			clearLoad();
			pausedRef.current = false;
			setPaused(false);
			return true;
		} catch (cause) {
			if (!signal.aborted) {
				pause(cause instanceof Error ? cause.message : "Couldn't change desktop control.");
				void read();
			}
			return false;
		} finally {
			actionLock.current = false;
			if (!signal.aborted) setBusy(false);
		}
	}, [accountId, clearLoad, endpoint, generation, pause, read, requestSignal, writer]);

	// Renew every 20s while we hold a live unexpired lease and the tab is
	// visible — through act so reads stay fenced; a failed renew pauses.
	useEffect(() => {
		const interval = window.setInterval(() => {
			if (!writer || pausedRef.current || actionLock.current || document.visibilityState !== "visible") return;
			if (!ownLease()) return;
			void act("human");
		}, 20000);
		return () => window.clearInterval(interval);
	}, [act, ownLease, writer]);

	const onInput = useCallback(async (input: WrenDesktopInput): Promise<boolean> => {
		const signal = requestSignal();
		// Synchronous guards — queued callbacks must not race a renew/action or
		// send while paused/lease-expired/generation-mismatched.
		if (signal.aborted || !writer || pausedRef.current || loadFailedRef.current || busy || actionLock.current) return false;
		const c = currentControl.current;
		if (!c || !ownLease()) return false;
		actionLock.current = true;
		readSequence.current++;
		try {
			await canterFetch(`${endpoint}/input`, {
				method: "POST",
				body: JSON.stringify({ ...input, expectedGeneration: generation, leaseToken: c.leaseToken }),
				signal,
			});
			return !signal.aborted;
		} catch (cause) {
			if (!signal.aborted) pause(cause instanceof Error ? cause.message : "Input didn't reach the desktop.");
			return false;
		} finally {
			actionLock.current = false;
		}
	}, [busy, endpoint, generation, ownLease, pause, requestSignal, writer]);

	// Best-effort release on unmount — independent short signal; refs stay
	// dead (no late repaints).
	useEffect(() => () => {
		const c = currentControl.current;
		if (!writer || !c || c.mode !== "human" || c.accountId !== accountId || !c.leaseToken) return;
		void fetch(`/api/canter${endpoint}/control`, {
			method: "POST", credentials: "include", keepalive: true,
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ mode: "agent", expectedGeneration: generation, leaseToken: c.leaseToken }),
			signal: AbortSignal.timeout(3000),
		}).catch(() => {});
	}, [accountId, endpoint, generation, writer]);

	// Render-safe derivation — from `control` state + `now`, never refs.
	const ownedLease = !!control && control.mode === "human" && control.accountId === accountId && !!control.leaseToken
		&& !!control.expiresAt && new Date(control.expiresAt).getTime() > now
		&& control.generation > 0 && control.generation === generation;
	const controlling = writer && ownedLease && !paused && !busy && !loadError && !!control;
	const otherHolder = control?.mode === "human" && control.accountId !== accountId;
	const statusText = control?.mode === "human"
		? `${control.name ?? "A member"} is controlling this desktop.`
		: paused ? "Desktop control paused — resume to continue." : "";

	return <section>
		<div className={styles.pageHeading} style={{ marginBottom: 12 }}>
			<div>
				<h2>Desktop</h2>
				<p>Everyone in this organization can watch. Return control before running commands or using the terminal. Browser logins and running apps aren’t included in saved environments.</p>
				{statusText ? <p role="status" className={styles.status}>{statusText}</p> : null}
			</div>
			{writer ? <div className={styles.pageActions}>
				{!otherHolder && control?.mode === "agent" ? <button className={styles.primaryButton} disabled={busy} onClick={() => void act("human")}>Take control</button> : null}
				{!otherHolder && ownedLease ? <>
					{paused ? <button className={styles.primaryButton} disabled={busy} onClick={() => void act("human")}>Resume control</button> : null}
					<button className={styles.secondaryButton} disabled={busy} onClick={() => void act("agent")}>Return control</button>
				</> : null}
			</div> : null}
		</div>
		{actionError ? <p className={styles.error} role="alert">{actionError}</p> : null}
		{loadError ? <p className={styles.status} role="status">{loadError}</p> : null}
		<WrenDesktop endpoint={endpoint} name={sandbox.name} controlling={controlling} onInput={onInput} revision={generation} />
	</section>;
}
