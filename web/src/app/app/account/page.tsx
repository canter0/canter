"use client";

import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { AppShell, SectionHeader } from "@/components/app-shell";
import { canterFetch, type Installation, type Me } from "@/lib/canter-api";

function SettingsRows({ rows }: { rows: string[][] }) {
  return <>{rows.map(([key, value]) => <div key={key} className="flex h-14 items-center justify-between border-b border-[var(--rule)]"><span>{key}</span><span className="text-[var(--muted)]">{value}</span></div>)}</>;
}

export default function AccountPage() {
  const router = useRouter();
  const [me, setMe] = useState<Me | null>(null);
  const [installations, setInstallations] = useState<Installation[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const identity = await canterFetch<Me>("/me");
        const workspace = identity.workspaces[0];
        const result = workspace ? await canterFetch<{ installations: Installation[] }>(`/installations?workspaceId=${encodeURIComponent(workspace.id)}`) : { installations: [] };
        if (!cancelled) { setMe(identity); setInstallations(result.installations); }
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "Canter could not load this account.");
      }
    })();
    return () => { cancelled = true; };
  }, []);

  async function signOut() {
    await canterFetch("/auth/signout", { method: "POST" });
    router.push("/sign-in");
    router.refresh();
  }

  const workspace = me?.workspaces[0];
  const activeInstallations = installations.filter((item) => !item.revokedAt);

  return (
    <AppShell active="Account">
      <section className="flex min-h-[calc(100vh-80px)] flex-col overflow-x-auto px-6 pt-10 sm:px-10 lg:px-14 lg:pt-11">
        <div className="min-w-[680px] border-b border-[var(--ink)] pb-7"><div className="meta">Human account</div><h1 className="display mt-3 text-[32px]">Account</h1></div>
        <div className="mt-10 grid min-w-[680px] gap-16 lg:grid-cols-2">
          <div><SectionHeader left="IDENTITY" /><SettingsRows rows={[["email", me?.account.email ?? "loading"], ["account", me?.account.id ?? "—"], ["password", "stored as a one-way hash"]]} /></div>
          <div><SectionHeader left="WORKSPACE" /><SettingsRows rows={[["name", workspace?.name ?? "loading"], ["role", "owner"], ["revision", String(workspace?.revision ?? "—")], ["agents", `${activeInstallations.length} authorized`]]} /></div>
        </div>
        <div className="mt-14 grid min-w-[680px] gap-16 lg:grid-cols-2">
          <div><SectionHeader left="WEB SESSION" right="CURRENT" /><SettingsRows rows={[["authentication", "HttpOnly session"], ["scope", "human account"]]} /></div>
          <div><SectionHeader left="ACCESS" /><div className="flex h-14 items-center justify-between border-b border-[var(--rule)]"><span>Beta access</span><span className="flex items-center gap-2"><span className="signal" />active</span></div></div>
        </div>
        {error ? <div className="mt-8 border-l-2 border-[var(--ink)] pl-4">{error}</div> : null}
        <div className="mt-auto flex min-h-20 min-w-[680px] items-center justify-end border-t border-[var(--ink)]"><button onClick={() => void signOut()} className="rule-link">Sign out ↗</button></div>
      </section>
    </AppShell>
  );
}
