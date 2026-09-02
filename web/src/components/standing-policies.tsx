"use client";

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { AppShell, Metric, SectionHeader } from "@/components/app-shell";
import { canterFetch, relativeTime, type ChangeDetail, type Installation, type Me, type StandingPolicy, type StandingPolicyEnvelope } from "@/lib/canter-api";

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
    const me = await canterFetch<Me>("/me");
    const workspace = me.workspaces[0];
    if (!workspace) throw new Error("No workspace is available.");
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
				const me = await canterFetch<Me>("/me");
				const workspace = me.workspaces[0];
				if (!workspace) throw new Error("No workspace is available.");
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
    if (!workspaceId || !envelope || hours < 1 || hours > 8760) return;
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

  return (
    <AppShell active="System" context={`canter / default / ${system} / policies`}>
      <section className="flex min-h-[calc(100vh-80px)] flex-col overflow-x-auto px-6 pt-10 sm:px-10 lg:px-14 lg:pt-11">
        <div className="grid min-w-[760px] items-end border-b border-[var(--ink)] pb-7 lg:grid-cols-[1fr_170px_170px]">
          <div><div className="meta">System / {system}</div><h1 className="display mt-3 text-[32px]">Standing policies</h1></div>
          <div className="mt-8 grid grid-cols-2 lg:contents"><Metric label="Active" value={String(active.length)} /><Metric label="Recorded" value={String(policies.length)} /></div>
        </div>
        {error ? <div className="mt-8 border-l-2 border-[var(--ink)] pl-4">{error}</div> : null}
        <div className="mt-10 min-w-[760px]"><SectionHeader left="DURABLE ENVELOPES" right={`${policies.length} RECORDED`} />
          {policies.map((policy) => {
            const status = policy.revokedAt ? "revoked" : new Date(policy.expiresAt).getTime() <= observedAt ? "expired" : "active";
			const scale = Object.entries(policy.envelope.scaleLimits ?? {}).map(([service, range]) => `${service} ${range.min}–${range.max}`).join(", ");
            return <div key={policy.id} className="grid min-h-20 grid-cols-[1.3fr_1fr_1fr_100px] items-center gap-5 border-b border-[var(--rule)] py-3"><div><div className="display text-[17px]">{policy.name}</div><div className="mt-1 text-[10px] text-[var(--muted)]">{policy.digest.slice(0, 16)} · rev {policy.systemRevision}</div></div><span>{scale || `${policy.envelope.operationKinds.length} operation kinds`} · ${(policy.envelope.maxAdditionalMonthlyCostCents / 100).toFixed(2)} max</span><span className="text-[var(--muted)]">{status === "active" ? `expires ${relativeTime(policy.expiresAt)}` : status}</span>{status === "active" ? <button disabled={pending} onClick={() => void revoke(policy)} className="rule-link text-right">{confirmRevoke === policy.id ? "Confirm revoke" : "Revoke"}</button> : <span className="text-right text-[var(--muted)]">{status}</span>}</div>;
          })}
          {!policies.length ? <div className="flex h-24 items-center border-b border-[var(--rule)] text-[var(--muted)]">No standing authority. Every Change requires exact human approval.</div> : null}
        </div>
        {change && envelope ? <div className="mt-14 grid min-w-[760px] gap-14 lg:grid-cols-[1.2fr_1fr]">
		  <div><SectionHeader left="DERIVED FROM CHANGE" right={change.id} />{[["services", envelope.affectedServices.join(", ")], ["operations", envelope.operationKinds.join(", ")], ["availability", envelope.availability.join(", ")], ["data", envelope.data.join(", ")], ["reversibility", envelope.allowedReversibility.join(", ")], ...(change.plan?.scale ? [["capacity", `${change.plan.scale.fromReplicas} → ${change.plan.scale.toReplicas} replicas`], ["duration", change.plan.scale.leaseSeconds ? `${change.plan.scale.leaseSeconds} seconds, then restore ${change.plan.scale.restoreToReplicas}` : "permanent"]] : [])].map(([key, value]) => <div key={key} className="flex min-h-12 items-center justify-between gap-8 border-b border-[var(--rule)] py-3"><span>{key}</span><span className="max-w-[520px] text-right text-[var(--muted)]">{value}</span></div>)}</div>
          <div><SectionHeader left="HUMAN BOUNDS" /><label className="block border-b border-[var(--rule)] py-3"><span className="meta">Policy name</span><input value={name} onChange={(event) => setName(event.target.value)} className="mt-2 w-full bg-transparent outline-none" /></label><label className="block border-b border-[var(--rule)] py-3"><span className="meta">Allowed installation</span><select value={installationId} onChange={(event) => setInstallationId(event.target.value)} className="mt-2 w-full bg-transparent outline-none">{installations.map((installation) => <option key={installation.id} value={installation.id}>{installation.name}</option>)}</select></label>{change.plan?.scale ? <div className="grid grid-cols-2 gap-5 border-b border-[var(--rule)] py-3"><label><span className="meta">Minimum replicas</span><input type="number" min={1} max={scaleMax} value={scaleMin} onChange={(event) => setScaleMin(Number(event.target.value))} className="mt-2 w-full bg-transparent outline-none" /></label><label><span className="meta">Maximum replicas</span><input type="number" min={scaleMin} value={scaleMax} onChange={(event) => setScaleMax(Number(event.target.value))} className="mt-2 w-full bg-transparent outline-none" /></label></div> : null}<label className="block border-b border-[var(--rule)] py-3"><span className="meta">Additional monthly cost ceiling · cents</span><input type="number" min={0} value={maxCost} onChange={(event) => setMaxCost(Number(event.target.value))} className="mt-2 w-full bg-transparent outline-none" /></label><label className="block border-b border-[var(--rule)] py-3"><span className="meta">Expires after · hours</span><input type="number" min={1} max={8760} value={hours} onChange={(event) => setHours(Number(event.target.value))} className="mt-2 w-full bg-transparent outline-none" /></label></div>
        </div> : <div className="mt-14 min-w-[760px] border-t border-[var(--ink)] py-8"><span className="text-[var(--muted)]">Policies begin from a real server-generated Change envelope.</span> <Link href="/app/changes" className="rule-link ml-2">Choose a Change ↗</Link></div>}
        <div className="mt-auto flex min-h-20 min-w-[760px] items-center justify-between border-t border-[var(--ink)]"><Link href={`/app/system/${encodeURIComponent(system)}`} className="rule-link">Back to System ↗</Link>{change && envelope ? <button onClick={() => void createPolicy()} disabled={pending || !name || !installationId} className="flex h-10 min-w-[210px] items-center justify-center bg-[var(--ink)] px-6 text-[var(--paper)] disabled:opacity-30">{pending ? "Creating…" : "Create exact envelope"}</button> : null}</div>
      </section>
    </AppShell>
  );
}
