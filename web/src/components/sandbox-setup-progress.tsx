"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { canterFetch } from "@/lib/canter-api";
import { useRequestScope } from "./use-request-scope";
import { followJob, type FollowState } from "@/lib/sandbox-client";
import styles from "./workspace.module.css";

// SandboxSetupProgress shows the durable setup state on a ready sandbox:
// fixed status/error plus the live setup job log. Every member can read the
// log; only a writer can cancel the exact active setup job. It is rendered
// alongside the ordinary command panel — never inside it.
export default function SandboxSetupProgress({
	base, setup, writer, available, onRefresh,
}: {
	base: string;
	setup: { status?: string; jobId?: string; error?: string; exitCode?: number };
	writer: boolean;
	available: boolean; // machine ready and not sleeping/deleting
	onRefresh: () => void;
}) {
	const { signal } = useRequestScope();
	const [log, setLog] = useState<FollowState>({ text: "", offset: 0, status: "running", truncated: false });
	const [retry, setRetry] = useState(0);
	const [stopping, setStopping] = useState(false);
	const [error, setError] = useState("");
	const stopRef = useRef<(() => void) | null>(null);
	useEffect(() => {
		if (!setup.jobId || !available) return;
		const controller = new AbortController();
		stopRef.current = followJob(base, setup.jobId, state => {
			if (!controller.signal.aborted) setLog(state);
		}, controller.signal);
		return () => {
			controller.abort();
			stopRef.current?.();
		};
	}, [base, setup.jobId, available, retry]);
	const stop = useCallback(async () => {
		if (!writer || !available || setup.status !== "running" || !setup.jobId || stopping) return;
		const requestSignal = signal();
		if (requestSignal.aborted) return;
		setStopping(true);
		setError("");
		try {
			await canterFetch(`${base}/jobs/${encodeURIComponent(setup.jobId)}/stop`, { method: "POST", signal: requestSignal });
			if (!requestSignal.aborted) onRefresh();
		} catch (cause) {
			if (!requestSignal.aborted) setError(cause instanceof Error ? cause.message : "Couldn't stop setup.");
		} finally {
			if (!requestSignal.aborted) setStopping(false);
		}
	}, [base, setup.jobId, setup.status, writer, available, stopping, signal, onRefresh]);
	const status = setup.status ?? "";
	return <section>
		<h2>Setup</h2>
		<p className={styles.status} data-phase={status === "succeeded" ? "succeeded" : status === "failed" ? "failed" : "running"}>
			{status === "pending" ? "Setup pending…" : status === "running" ? "Setup running…" : status === "succeeded" ? "Setup completed" : status === "failed" ? `Setup failed: ${setup.error ?? "did not complete successfully"}` : "Setup"}
		</p>
		{status === "failed" ? <p>The sandbox is retained for inspection and repair.</p> : null}
		{setup.jobId ? <pre style={{ maxHeight: 320, overflow: "auto", fontFamily: "monospace", fontSize: 12, padding: 12, background: "#141414", borderRadius: 8 }}>{log.text || "Waiting for setup output…"}{log.truncated ? "\n…earlier output truncated" : ""}</pre> : null}
		{log.status && setup.jobId ? <p className={styles.muted}>job {setup.jobId} · {log.status}</p> : null}
		{error ? <p role="alert" className={styles.error}>{error}</p> : null}
		{log.error ? <p role="alert" className={styles.error}>{log.error}</p> : null}
		{writer && status === "running" && setup.jobId && available ? <button className={styles.secondaryButton} disabled={stopping} onClick={() => void stop()}>{stopping ? "Stopping…" : "Stop setup"}</button> : null}
		{setup.jobId ? <button className={styles.secondaryButton} disabled={!available} onClick={() => setRetry(r => r + 1)}>Reconnect log</button> : null}
	</section>;
}
