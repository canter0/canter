"use client";
import {getSelectedWorkspace} from "@/lib/workspace-selection";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { AuthTitle } from "@/components/auth-shell";
import { canterFetch, CanterAPIError, type DeviceAuthorization } from "@/lib/canter-api";

export function AuthorizeAgent({ code }: { code: string }) {
  const router = useRouter();
  const [request, setRequest] = useState<DeviceAuthorization | null>(null);
  const [workspaceId, setWorkspaceId] = useState("");
  const [workspaceName,setWorkspaceName]=useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const [signInRequired, setSignInRequired] = useState(false);
  const [reauthRequired, setReauthRequired] = useState(false);
  const reviewPath = `/onboarding/authorize?code=${encodeURIComponent(code)}`;

  useEffect(() => {
    let cancelled = false;
    void Promise.all([
      canterFetch<DeviceAuthorization>(`/device/authorizations/${encodeURIComponent(code)}`),
      getSelectedWorkspace(),
    ]).then(([authorization, me]) => {
      if (cancelled) return;
      setRequest(authorization);
      setWorkspaceId(me.workspace.id);
      setWorkspaceName(me.workspace.name);
    }).catch((cause) => {
      if (cancelled) return;
      if (cause instanceof CanterAPIError && cause.status === 401) {
        setSignInRequired(true);
        return;
      }
      setError(cause instanceof Error ? cause.message : "Canter could not load this request.");
    });
    return () => { cancelled = true; };
  }, [code]);

  async function decide(decision: "approve" | "deny") {
    if (pending || !request || request.status !== "pending" || (decision === "approve" && (!workspaceId || reauthRequired))) return;
    setPending(true);
    setError("");
    try {
      await canterFetch(`/device/authorizations/${encodeURIComponent(code)}/${decision}`, {
        method: "POST",
        body: JSON.stringify(decision === "approve" ? { workspaceId } : {}),
      });
      router.push(decision === "approve" ? "/app/agents" : "/onboarding/agent");
      router.refresh();
    } catch (cause) {
      if (cause instanceof CanterAPIError && decision === "approve" && cause.status === 428) {
        setReauthRequired(true);
      } else if (cause instanceof CanterAPIError && cause.status === 410) {
        setRequest(current => current ? { ...current, status: "expired" } : current);
      } else if (cause instanceof CanterAPIError && cause.status === 409) {
        const latest = await canterFetch<DeviceAuthorization>(`/device/authorizations/${encodeURIComponent(code)}`).catch(() => null);
        if (latest && latest.status !== "pending") {
          setRequest(latest);
        } else {
          setError(cause instanceof Error ? cause.message : "Canter could not record that decision.");
        }
      } else {
        setError(cause instanceof Error ? cause.message : "Canter could not record that decision.");
      }
      setPending(false);
    }
  }

  const authority = request ? [
    ["Inspect systems", request.authority.inspect ? "allowed" : "not requested"],
    ["Draft Changes", request.authority.draft ? "allowed" : "not requested"],
    ["Apply Changes", request.authority.applyMode || "approval required"],
  ] : [];

  if (signInRequired) {
    return (
      <>
        <AuthTitle title="Sign in to review." description="This agent request is waiting for a human-authenticated decision." />
        <div className="mt-10 grid grid-cols-2 gap-3">
          <Link href={`/sign-in?next=${encodeURIComponent(reviewPath)}`} className="flex h-11 items-center justify-center bg-[var(--ink)] text-[var(--paper)]">Sign in</Link>
          <Link href={`/create-account?next=${encodeURIComponent(reviewPath)}`} className="flex h-11 items-center justify-center border border-[var(--ink)]">Create account</Link>
        </div>
        <p className="mt-7 text-[var(--muted)]">The agent cannot approve its own request. Canter will return you here after authentication.</p>
      </>
    );
  }

  const ready = request?.status === "pending";
  if (request && !ready) {
    return (
      <>
        <AuthTitle title={request.status === "expired" ? "This authorization request expired." : "This authorization request has ended."} description="Ask your agent to start a new authorization request, then open its new link." />
        <div className="mt-10">
          <Link href="/onboarding/agent" className="flex h-11 items-center justify-center border border-[var(--ink)]">Back to agent setup</Link>
        </div>
      </>
    );
  }

  return (
    <>
      <AuthTitle title={`Authorize ${request?.name ?? "agent"}.`} description={request ? `${request.name} (${request.harness}) is asking to operate Canter in ${workspaceName}.` : "Loading the agent's request."} />
      {error ? <p role="alert" className="mt-8 border-l-2 border-[var(--ink)] pl-4 leading-5">{error}</p> : null}
      <div className="mt-10 border-t border-[var(--ink)]">
        {authority.map(([permission, value]) => <div key={permission} className="flex h-14 items-center justify-between border-b border-[var(--rule)]"><span>{permission}</span><span className="text-[var(--muted)]">{value}</span></div>)}
      </div>
      {reauthRequired ? <p role="status" className="mt-8 text-[var(--muted)]">Confirm your identity to continue. Canter will return you here to review and authorize this request.</p> : null}
      <div className="mt-10 grid grid-cols-2 gap-3">
        {reauthRequired
          ? <Link href={`/sign-in?reauth=1&next=${encodeURIComponent(reviewPath)}`} className="flex h-11 items-center justify-center bg-[var(--ink)] text-[var(--paper)]">Confirm your identity</Link>
          : <button onClick={() => void decide("approve")} disabled={pending || !request || !workspaceId} className="h-11 bg-[var(--ink)] text-[var(--paper)] disabled:opacity-60">{pending ? "Recording…" : "Authorize"}</button>}
        <button onClick={() => void decide("deny")} disabled={pending || !request} className="h-11 border border-[var(--ink)] disabled:opacity-60">Deny</button>
      </div>
      <button onClick={() => router.push("/onboarding/agent")} className="rule-link mt-7 text-[var(--muted)]">This is not your agent? ↗</button>
    </>
  );
}
