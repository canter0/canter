"use client";

import { useEffect, useState } from "react";
import { canterFetch } from "@/lib/canter-api";
import styles from "./vps-review.module.css";

type VPS = {
  id: string; name: string; phase: string; digest: string;
  vcpus: number; memoryMiB: number; diskGiB: number; os: string;
  monthlyCents: number; estimatedCents?: number; forSeconds: number;
  sshCidr: string; sshPublicKey: string; username: string; address: string;
  expiresAt: string | null; hostFingerprint: string; failure: string; deleteRequested?: boolean;
};
const phaseLabel: Record<string, string> = { drafted: "Ready for review", queued: "Waiting to start", creating: "Preparing your server", ready: "Ready", deleting: "Deleting server", deleted: "Deleted" };
const duration = (seconds: number) => seconds % 3600 === 0 ? `${seconds / 3600} hour${seconds === 3600 ? "" : "s"}` : `${Math.round(seconds / 60)} minutes`;
const cost = (cents: number) => cents < 100 ? `${Number(cents.toFixed(2))}¢` : `$${(cents / 100).toFixed(2)}`;

export function VPSReview({ id, workspaceId }: { id: string; workspaceId: string }) {
  const [vm, setVM] = useState<VPS | null>(null);
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [pending, setPending] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const base = `/workspaces/${encodeURIComponent(workspaceId)}/vps/${encodeURIComponent(id)}`;
  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    async function refresh() {
      try {
        const next = await canterFetch<VPS>(base, { signal: controller.signal });
        if (controller.signal.aborted) return;
        setVM(next); setError("");
        if (next.phase === "deleted") return;
      } catch (cause) {
        if (controller.signal.aborted) return;
        setError(cause instanceof Error ? cause.message : "Could not refresh your server.");
      }
      timer = setTimeout(() => void refresh(), 3000);
    }
    void refresh();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [base, attempt]);

  async function act(action: "approve" | "delete") {
    if (!vm || pending) return;
    setPending(true); setActionError("");
    try {
      await canterFetch(base + "/" + action, { method: "POST", body: JSON.stringify(action === "approve" ? { digest: vm.digest } : {}) });
      setConfirmDelete(false);
      setAttempt(value => value + 1);
    } catch (cause) { setActionError(cause instanceof Error ? cause.message : "Could not update your server."); }
    finally { setPending(false); }
  }
  return <section className={styles.card} aria-label="VPS review">
    {actionError ? <p role="alert" className={styles.error}>{actionError}</p> : null}
    {error ? <p role="alert" className={styles.error}>{error} <button onClick={() => setAttempt(value => value + 1)}>Retry</button></p> : null}
    {!vm ? <p role="status">Loading your server…</p> : <>
      <header><h2>{vm.name}</h2><span role="status">{vm.deleteRequested && vm.phase !== "deleted" ? "Deletion requested" : phaseLabel[vm.phase] ?? vm.phase}</span></header>
      <p>{vm.vcpus} vCPU · {vm.memoryMiB / 1024} GiB RAM · {vm.diskGiB} GiB disk</p>
      <p className={styles.muted}>{vm.os}</p>
      <div className={styles.price}><strong>{vm.estimatedCents === undefined ? cost(vm.monthlyCents) : cost(vm.estimatedCents)}</strong><span>{vm.forSeconds ? `estimated for ${duration(vm.forSeconds)}` : "per 720 hours"}</span></div>
      <p className={styles.muted}>Canter compute usage, including local disk and one public IPv4. Taxes and separate storage are additional; applicable plan credits may reduce the bill.</p>
      <dl><dt>SSH user</dt><dd>{vm.username}</dd><dt>Allowed source</dt><dd>{vm.sshCidr}</dd><dt>Lifetime</dt><dd>{vm.forSeconds ? `${duration(vm.forSeconds)} from approval, including setup time` : "Until you delete it"}</dd></dl>
      {vm.expiresAt && vm.phase !== "deleted" ? <p>Scheduled deletion: <strong>{new Date(vm.expiresAt).toLocaleString()}</strong></p> : null}
      {vm.failure ? <p role="status" className={styles.error}>{vm.failure}</p> : null}
      {vm.phase === "drafted" ? <>
        <details><summary>Review SSH public key</summary><code className={styles.key}>{vm.sshPublicKey}</code></details>
        <p>{vm.forSeconds ? "Approving starts billed provisioning and schedules permanent deletion of this server and its local data at expiry. Save anything you need before then." : "Approving starts billed provisioning. Compute remains allocated until deletion is verified, including when the VM is stopped."}</p>
        <button className={styles.primary} disabled={pending} onClick={() => void act("approve")}>{pending ? "Submitting…" : "Approve and create VPS"}</button>
      </> : null}
      {vm.phase === "ready" && !vm.deleteRequested ? <>
        <h3>Connect</h3><code className={styles.command}>ssh {vm.username}@{vm.address}</code>
        <p className={styles.muted}>Boot and SSH setup verified. Connect using the private key matching your public key.</p>
        <p className={styles.muted}>SSH host fingerprint</p><code className={styles.key}>{vm.hostFingerprint}</code>
      </> : null}
      {!["drafted", "deleted", "deleting"].includes(vm.phase) && !vm.deleteRequested ? <div className={styles.delete}>
        {confirmDelete ? <><p>Delete this server and permanently erase its local data?</p><button disabled={pending} onClick={() => void act("delete")}>{pending ? "Requesting deletion…" : "Delete server and data"}</button><button disabled={pending} onClick={() => setConfirmDelete(false)}>Keep server</button></> : <button disabled={pending} onClick={() => setConfirmDelete(true)}>Delete server…</button>}
      </div> : null}
      {vm.phase === "deleted" ? <p>Removal verified. This server no longer has an active compute allocation.</p> : null}
    </>}
  </section>;
}
