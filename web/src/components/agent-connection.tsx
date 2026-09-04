"use client";

import { useRouter } from "next/navigation";
import { type FormEvent, useState } from "react";
import { canterFetch, type DeviceAuthorization } from "@/lib/canter-api";

export function AgentConnection() {
  const router = useRouter();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");

  async function review(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setPending(true);
    setError("");
    const data = new FormData(event.currentTarget);
    const code = String(data.get("code") ?? "").trim().toUpperCase();
    try {
      const request = await canterFetch<DeviceAuthorization>(`/device/authorizations/${encodeURIComponent(code)}`);
      if (request.status !== "pending") throw new Error(`This connection is ${request.status}.`);
      router.push(`/onboarding/authorize?code=${encodeURIComponent(code)}`);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not find that connection.");
      setPending(false);
    }
  }

  return (
    <form onSubmit={review} className="mt-12">
      <label className="block border-y border-[var(--rule)] py-5">
        <span className="meta block">CONNECTION CODE</span>
        <div className="mt-3 flex items-center gap-4">
          <input name="code" required autoCapitalize="characters" autoComplete="off" placeholder="CANTER-••••" className="min-w-0 flex-1 bg-transparent text-[16px] uppercase tracking-[0.08em] outline-none placeholder:text-[var(--muted)]" />
          <span className="signal" />
        </div>
      </label>
      {error ? <p role="alert" className="mt-5 border-l-2 border-[var(--ink)] pl-4 leading-5">{error}</p> : null}
      <button disabled={pending} className="mt-8 h-11 w-full bg-[var(--ink)] text-[var(--paper)] disabled:opacity-60">{pending ? "Finding agent…" : "Review agent"}</button>
      <div className="mt-7 text-[var(--muted)]">Ask your agent to connect to Canter. Enter the one-time code it returns.</div>
    </form>
  );
}
