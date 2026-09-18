"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useRef, useState } from "react";
import { canterFetch } from "@/lib/canter-api";
import { authDestination, authError, type AuthProviders } from "@/lib/auth";
import { ProviderIcon } from "./provider-icon";

const inputClass = "h-9 w-full rounded-[5px] border border-[#363636] bg-[#222] px-3 text-[14px] outline-none transition-colors placeholder:text-[#858585] focus:border-[#4aaaf0] focus:ring-1 focus:ring-[#4aaaf0]";

export function AuthForm({ mode, next = "", initialError = "" }: { mode: "sign-in" | "create-account"; next?: string; initialError?: string }) {
  const router = useRouter();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState(authError(initialError));
  const [passwordStep, setPasswordStep] = useState(false);
  const [email, setEmail] = useState("");
  const [providers, setProviders] = useState<AuthProviders | null>(null);
  const passwordRef = useRef<HTMLInputElement>(null);
  const create = mode === "create-account";
  const destination = authDestination(next, "/app");

  useEffect(() => {
    let cancelled = false;
    canterFetch<AuthProviders>("/auth/providers").then(value => { if (!cancelled) setProviders(value); }).catch(() => { if (!cancelled) setError("We couldn't reach Canter. Please refresh and try again."); });
    return () => { cancelled = true; };
  }, []);

  useEffect(() => { if (passwordStep) passwordRef.current?.focus(); }, [passwordStep]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    if (!passwordStep) { setPasswordStep(true); return; }
    setPending(true);
    const data = new FormData(event.currentTarget);
    try {
      await canterFetch(create ? "/auth/signup" : "/auth/signin", {
        method: "POST",
        body: JSON.stringify({ email, password: String(data.get("password") ?? "") }),
      });
      router.push(destination);
      router.refresh();
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : "We couldn't complete your request.";
      setError(message === "unauthorized" ? "The email or password is incorrect." : message);
      setPending(false);
    }
  }

  function startProvider(event: React.MouseEvent<HTMLAnchorElement>, enabled: boolean) {
    if (pending || !enabled) { event.preventDefault(); return; }
    setPending(true);
  }

  return (
    <div className="text-[14px]">
      <div className="grid gap-2">
        {(["github", "google"] as const).map(provider => {
          const enabled = providers?.providers.some(item => item.id === provider && item.enabled) ?? false;
          const query = new URLSearchParams({ mode, next: destination });
          return <a key={provider} href={enabled ? `/api/canter/auth/oauth/${provider}?${query}` : undefined} onClick={event => startProvider(event, enabled)} aria-disabled={pending || !enabled} tabIndex={pending || !enabled ? -1 : 0} title={providers && !enabled ? "Sign-in is being configured" : undefined} className="relative flex h-9 w-full items-center justify-center rounded-[5px] border border-[#363636] bg-[#222] transition-colors hover:bg-[#2a2a2a] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#4aaaf0] aria-disabled:cursor-not-allowed aria-disabled:opacity-50"><span className="absolute left-3"><ProviderIcon provider={provider}/></span>Continue with {provider === "github" ? "GitHub" : "Google"}</a>;
        })}
      </div>
      <div className="my-6 flex items-center gap-3 text-[10px] text-[#7b7b7b]"><span className="h-px flex-1 bg-[#252525]"/>OR<span className="h-px flex-1 bg-[#252525]"/></div>
      <form onSubmit={submit} className="grid gap-2">
        <input aria-label="Email address" name="email" type="email" autoComplete="email" placeholder="Email address" required value={email} onChange={event => setEmail(event.target.value)} readOnly={pending} className={inputClass}/>
        {passwordStep ? <>
          <input ref={passwordRef} aria-label="Password" name="password" type="password" autoComplete={create ? "new-password" : "current-password"} placeholder={create ? "Create a password" : "Password"} required minLength={create ? 12 : undefined} maxLength={1024} disabled={pending} className={inputClass}/>
          {create ? <p className="mb-1 text-[12px] text-[#888]">Use at least 12 characters.</p> : null}
        </> : null}
        {error ? <p role="alert" className="py-2 text-[13px] leading-5 text-[#f2a5a5]">{error}</p> : null}
        <button disabled={pending || !providers} style={{ color: "#111" }} className="h-9 rounded-[5px] border border-[#d1d1d1] bg-[#e5e5e5] transition-colors hover:bg-white focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#4aaaf0] disabled:opacity-60">{pending ? "Please wait…" : !passwordStep ? "Continue with email" : create ? "Sign up" : "Log in"}</button>
      </form>
      <p className="mt-6 text-center text-[#888]">{create ? "Already have an account?" : "Don't have an account?"}{" "}<Link href={`${create ? "/sign-in" : "/create-account"}${next ? `?next=${encodeURIComponent(destination)}` : ""}`} style={{ color: "#4aaaf0" }} className="hover:underline">{create ? "Log in" : "Sign up"}</Link></p>
    </div>
  );
}
