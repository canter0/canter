"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { type ReactNode, useEffect, useState } from "react";
import { authorityLabel, canterFetch, CanterAPIError, relativeTime, type Installation, type Me } from "@/lib/canter-api";

type NavItem = "System" | "Changes" | "Agents" | "Account";
const navigation: Array<{ label: Exclude<NavItem, "Account">; href: string }> = [
  { label: "System", href: "/app" },
  { label: "Changes", href: "/app/changes" },
  { label: "Agents", href: "/app/agents" },
];

export function AppShell({ active, context = "canter / default", children }: { active: NavItem; context?: string; children: ReactNode }) {
  const router = useRouter();
  const [workspaceName, setWorkspaceName] = useState("default");
  const [installation, setInstallation] = useState<Installation | null>(null);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const me = await canterFetch<Me>("/me");
        const workspace = me.workspaces[0];
        if (!workspace || cancelled) return;
        setWorkspaceName(workspace.name);
        const result = await canterFetch<{ installations: Installation[] }>(`/installations?workspaceId=${encodeURIComponent(workspace.id)}`);
        if (!cancelled) setInstallation(result.installations.find((item) => !item.revokedAt) ?? null);
      } catch (error) {
        if (!cancelled && error instanceof CanterAPIError && error.status === 401) router.replace("/sign-in");
      }
    })();
    return () => { cancelled = true; };
  }, [router]);

  const agentStatus = installation
    ? `${installation.name} authorized · ${authorityLabel(installation.authority)} · seen ${relativeTime(installation.lastSeenAt)}`
    : "No agent authorized";

  return (
    <div className="min-h-screen bg-[var(--paper)] lg:grid lg:grid-cols-[216px_1fr]">
      <aside className="border-b border-[var(--rule)] px-6 py-6 lg:flex lg:h-screen lg:flex-col lg:border-r lg:border-b-0 lg:px-8 lg:py-9">
        <div className="flex items-center justify-between lg:block">
          <Link href="/" className="wordmark text-[24px] leading-none">canter</Link>
          <div className="flex items-center gap-2 lg:hidden"><span className="signal" /><span>{installation?.name ?? "No agent"}</span></div>
        </div>
        <nav className="mt-8 flex gap-5 lg:mt-24 lg:flex-col lg:gap-7">
          {navigation.map((item) => (
            <Link key={item.label} href={item.href} className={`flex items-center gap-3 ${active === item.label ? "text-[var(--ink)]" : "text-[var(--muted)]"}`}>
              <span className={`signal ${active === item.label ? "opacity-100" : "opacity-0"}`} />{item.label}
            </Link>
          ))}
        </nav>
        <div className="mt-8 hidden text-[11px] text-[var(--muted)] lg:mt-auto lg:block">
          <div>{workspaceName} workspace</div>
          <Link href="/app/account" className="rule-link mt-4 inline-block text-[var(--ink)]">Account ↗</Link>
        </div>
      </aside>
      <main className="min-w-0">
        <header className="hidden h-20 items-center justify-between border-b border-[var(--rule)] px-8 text-[11px] lg:flex lg:px-14">
          <div className="flex items-center gap-4"><span className="text-[var(--muted)]">WORKSPACE</span><span>{context === "canter / default" ? `canter / ${workspaceName}` : context}</span></div>
          <div className="flex items-center gap-3"><span className={`signal ${installation ? "" : "opacity-20"}`} /><span>{agentStatus}</span></div>
        </header>
        {children}
      </main>
    </div>
  );
}

export function Metric({ label, value }: { label: string; value: string }) {
  return <div className="border-l border-[var(--rule)] pl-5"><div className="meta">{label}</div><div className="mt-2 text-[22px]">{value}</div></div>;
}

export function SectionHeader({ left, right }: { left: string; right?: string }) {
  return <div className="flex justify-between border-b border-[var(--ink)] pb-3 text-[10px] tracking-[0.075em]"><span>{left}</span>{right ? <span>{right}</span> : null}</div>;
}
