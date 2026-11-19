"use client";
import {getSelectedWorkspace} from "@/lib/workspace-selection";

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { AppShell } from "@/components/app-shell";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./detail-page.module.css";
import { phaseTone } from "@/lib/workspace-overview";
import { canterFetch, relativeTime, type ChangeDetail, type Installation, type StandingPolicy, type StandingPolicyEnvelope } from "@/lib/canter-api";

function unique(values: Array<string | undefined>) {
  return [...new Set(values.filter((value): value is string => Boolean(value)))].sort();
}

export function StandingPolicies({ system, fromChange }: { system: string; fromChange?: string }) {
  const [workspaceId, setWorkspaceId] = useState("");
  const [policies, setPolicies] = useState<StandingPolicy[]>([]);
  const [installations, setInstallations] = useState<Installation[]>([]);
  const [change, setChange] = useState<ChangeDetail | null>(null);
  const [installationId, setInstallationId] = useState("");
  const [name, setName] = useState("bounded-release");
  const [hours, setHours] = useState(2);
  const [maxCost, setMaxCost] = useState(0);
  const [scaleMin, setScaleMin] = useState(1);
  const [scaleMax, setScaleMax] = useState(1);
  const [pending, setPending] = useState(false);
  const [confirmRevoke, setConfirmRevoke] = useState("");
  const [error, setError] = useState("");
	const [observedAt] = useState(() => Date.now());

  async function load(cancelled?: () => boolean) {
    const {workspace}=await getSelectedWorkspace();
    if (!workspace) throw new Error("No organization is available.");
    const [policyResult, agentResult, changeResult] = await Promise.all([
      canterFetch<{ policies: StandingPolicy[] }>(`/workspaces/${workspace.id}/systems/${encodeURIComponent(system)}/policies`),
      canterFetch<{ installations: Installation[] }>(`/installations?workspaceId=${encodeURIComponent(workspace.id)}`),
      fromChange ? canterFetch<ChangeDetail>(`/workspaces/${workspace.id}/systems/${encodeURIComponent(system)}/changes/${encodeURIComponent(fromChange)}`) : Promise.resolve(null),
    ]);
    if (cancelled?.()) return;
    const activeInstallations = agentResult.installations.filter((item) => !item.revokedAt);
    setWorkspaceId(workspace.id);
    setPolicies(policyResult.policies);
    setInstallations(activeInstallations);
    setInstallationId((current) => current || activeInstallations[0]?.id || "");
    setChange(changeResult);
    setMaxCost(Math.max(0, changeResult?.plan?.impact?.monthlyCostDeltaCents ?? 0));
	if (changeResult?.plan?.scale) {
	  setScaleMin(Math.min(changeResult.plan.scale.fromReplicas, changeResult.plan.scale.toReplicas));
	  setScaleMax(Math.max(changeResult.plan.scale.fromReplicas, changeResult.plan.scale.toReplicas));
	}
  }

  useEffect(() => {
    let cancelled = false;
		void (async () => {
			try {
				const {workspace}=await getSelectedWorkspace();
				if (!workspace) throw new Error("No organization is available.");
				const [policyResult, agentResult, changeResult] = await Promise.all([
					canterFetch<{ policies: StandingPolicy[] }>(`/workspaces/${workspace.id}/systems/${encodeURIComponent(system)}/policies`),
					canterFetch<{ installations: Installation[] }>(`/installations?workspaceId=${encodeURIComponent(workspace.id)}`),
					fromChange ? canterFetch<ChangeDetail>(`/workspaces/${workspace.id}/systems/${encodeURIComponent(system)}/changes/${encodeURIComponent(fromChange)}`) : Promise.resolve(null),
				]);
				if (cancelled) return;
				const activeInstallations = agentResult.installations.filter((item) => !item.revokedAt);
				setWorkspaceId(workspace.id);
				setPolicies(policyResult.policies);
				setInstallations(activeInstallations);
				setInstallationId(activeInstallations[0]?.id || "");
				setChange(changeResult);
				setMaxCost(Math.max(0, changeResult?.plan?.impact?.monthlyCostDeltaCents ?? 0));
				if (changeResult?.plan?.scale) {
					setScaleMin(Math.min(changeResult.plan.scale.fromReplicas, changeResult.plan.scale.toReplicas));
					setScaleMax(Math.max(changeResult.plan.scale.fromReplicas, changeResult.plan.scale.toReplicas));
				}
			} catch (cause) {
				if (!cancelled) setError(cause instanceof Error ? cause.message : "Canter could not load standing policies.");
			}
		})();
    return () => { cancelled = true; };
  }, [system, fromChange]);

  const envelope = useMemo<StandingPolicyEnvelope | null>(() => {
    if (!change || !installationId) return null;
    return {
      allowedInstallationIds: [installationId],
		affectedServices: unique(change.plan?.impact?.affectedServices ?? []),
      operationKinds: unique(change.operations?.map((operation) => operation.kind) ?? []),
      availability: unique([change.plan?.impact?.availability]),
      data: unique([change.plan?.impact?.data]),
      allowedReversibility: unique(change.operations?.map((operation) => operation.reversibility) ?? []),
      maxAdditionalMonthlyCostCents: maxCost,
      maxOperations: change.operations?.length ?? 0,
	  scaleLimits: change.plan?.scale ? { [change.plan.scale.service]: { min: scaleMin, max: scaleMax } } : undefined,
	  maxScaleDurationSeconds: change.plan?.scale?.leaseSeconds || undefined,
	  allowPermanentScale: Boolean(change.plan?.scale && !change.plan.scale.leaseSeconds),
    };
  }, [change, installationId, maxCost, scaleMin, scaleMax]);

  async function createPolicy() {
    if (!workspaceId || !envelope || pending || hours < 1 || hours > 8760) return;
    setPending(true);
    setError("");
    try {
      await canterFetch(`/workspaces/${workspaceId}/systems/${encodeURIComponent(system)}/policies`, {
        method: "POST",
        body: JSON.stringify({
          name,
          description: `Derived from Change ${change?.id}; permits only the displayed server-generated envelope.`,
          envelope,
          expiresAt: new Date(Date.now() + hours * 60 * 60 * 1000).toISOString(),
        }),
      });
      await load();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not create this policy.");
    } finally {
      setPending(false);
    }
  }

  async function revoke(policy: StandingPolicy) {
    if (pending) return;
    if (!workspaceId || confirmRevoke !== policy.id) {
      setConfirmRevoke(policy.id);
      return;
    }
    setPending(true);
    setError("");
    try {
      await canterFetch(`/workspaces/${workspaceId}/systems/${encodeURIComponent(system)}/policies/${encodeURIComponent(policy.id)}/revoke`, { method: "POST" });
      setConfirmRevoke("");
      await load();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not revoke this policy.");
    } finally {
      setPending(false);
    }
  }

	const active = policies.filter((policy) => !policy.revokedAt && new Date(policy.expiresAt).getTime() > observedAt);

  const derived: Array<[string, string]> = change && envelope ? [["Services", envelope.affectedServices.join(", ")], ["Operations", envelope.operationKinds.join(", ")], ["Availability", envelope.availability.join(", ")], ["Data", envelope.data.join(", ")], ["Reversibility", envelope.allowedReversibility.join(", ")], ...(change.plan?.scale ? [["Capacity", `${change.plan.scale.fromReplicas} → ${change.plan.scale.toReplicas} replicas`], ["Duration", change.plan.scale.leaseSeconds ? `${change.plan.scale.leaseSeconds} seconds, then restore ${change.plan.scale.restoreToReplicas}` : "Permanent"]] as Array<[string, string]> : [])] : [];

  return (
    <AppShell active="System" context={`canter / default / ${system} / policies`} pageTitle={`${system} policies`}>
      <section className={styles.page}><div className={styles.inner}>
        <header className={styles.header}>
          <div><div className={styles.eyebrow}>{system}</div><h1>Standing policies</h1><p>Bounded authority that lets matching changes proceed without a separate approval.</p></div>
        </header>
        {error ? <div role="alert" className={styles.error}>{error}</div> : null}
        <div className={styles.metrics}>
          <div><span>Active</span><strong>{active.length}</strong></div>
          <div><span>Recorded</span><strong>{policies.length}</strong></div>
        </div>
        <section className={styles.section}><h2>Policies<small>{policies.length}</small></h2>
          {policies.length ? <ul className={styles.list}>{policies.map((policy) => {
            const status = policy.revokedAt ? "revoked" : new Date(policy.expiresAt).getTime() <= observedAt ? "expired" : "active";
            const scale = Object.entries(policy.envelope.scaleLimits ?? {}).map(([service, range]) => `${service} ${range.min}–${range.max}`).join(", ");
            return <li key={policy.id}>
              <div>{policy.name}<small>{scale || `${policy.envelope.operationKinds.length} operation kinds`} · ${(policy.envelope.maxAdditionalMonthlyCostCents / 100).toFixed(2)} USD/month max · <span className={styles.mono}>{policy.digest.slice(0, 16)}</span> · rev {policy.systemRevision}</small></div>
              <div className={styles.buttons}>
                <span className={styles.stepPhase} data-tone={phaseTone(status)}>{status === "active" ? `Expires ${relativeTime(policy.expiresAt)}` : status.charAt(0).toUpperCase() + status.slice(1)}</span>
                {status === "active" ? <button disabled={pending} onClick={() => void revoke(policy)} className={styles.textButton} data-confirm={confirmRevoke === policy.id}>{confirmRevoke === policy.id ? "Confirm revoke" : "Revoke"}</button> : null}
              </div>
            </li>;
          })}</ul> : <p className={styles.empty}>No standing authority. Every change requires exact human approval.</p>}
        </section>
        {change && envelope ? <div className={`${styles.grid} ${styles.spaced}`}>
          <section className={styles.section}><h2>Derived from change<small className={styles.mono}>{change.id}</small></h2>
            <dl className={styles.rows}>{derived.map(([key, value]) => <div key={key}><dt>{key}</dt><dd>{value || "—"}</dd></div>)}</dl>
          </section>
          <section className={styles.section}><h2>Your bounds</h2>
            <div className={styles.fields}>
              <label><span>Policy name</span><input value={name} onChange={(event) => setName(event.target.value)} /></label>
              <label><span>Allowed installation</span><select value={installationId} onChange={(event) => setInstallationId(event.target.value)}>{installations.map((installation) => <option key={installation.id} value={installation.id}>{installation.name}</option>)}</select></label>
              {change.plan?.scale ? <div className={styles.pair}><label><span>Minimum replicas</span><input type="number" min={1} max={scaleMax} value={scaleMin} onChange={(event) => setScaleMin(Number(event.target.value))} /></label><label><span>Maximum replicas</span><input type="number" min={scaleMin} value={scaleMax} onChange={(event) => setScaleMax(Number(event.target.value))} /></label></div> : null}
              <label><span>Additional monthly cost ceiling (USD cents)</span><input type="number" min={0} value={maxCost} onChange={(event) => setMaxCost(Number(event.target.value))} /></label>
              <label><span>Expires after (hours)</span><input type="number" min={1} max={8760} value={hours} onChange={(event) => setHours(Number(event.target.value))} /></label>
            </div>
          </section>
        </div> : <p className={styles.notice}>Policies start from a real server-generated change. <Link href="/app/changes" className={styles.link}>Choose a change<WorkspaceIcon name="right" width="14" height="14" /></Link></p>}
        <div className={styles.actions}>
          <Link href={`/app/system/${encodeURIComponent(system)}`} className={styles.link}>Back to {system}</Link>
          {change && envelope ? <div className={styles.buttons}><button onClick={() => void createPolicy()} disabled={pending || !name || !installationId} className={styles.primary}>{pending ? "Creating…" : "Create policy"}</button></div> : null}
        </div>
      </div></section>
    </AppShell>
  );
}
