"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useState } from "react";
import { canterFetch } from "@/lib/canter-api";

export function AuthForm({ mode, next = "" }: { mode: "sign-in" | "create-account"; next?: string }) {
  const router = useRouter();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const create = mode === "create-account";

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    setPending(true);
    const data = new FormData(event.currentTarget);
    const password = String(data.get("password") ?? "");
    if (create && password !== String(data.get("confirmPassword") ?? "")) {
      setPending(false);
      setError("Passwords do not match.");
      return;
    }
    try {
      await canterFetch(create ? "/auth/signup" : "/auth/signin", {
        method: "POST",
        body: JSON.stringify({
          email: String(data.get("email") ?? ""),
          password,
        }),
      });
      const destination = next.startsWith("/") && !next.startsWith("//")
        ? next
        : create ? "/onboarding/agent" : "/app";
      router.push(destination);
      router.refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not complete the request.");
      setPending(false);
    }
  }

  const fields = create
    ? [
        { name: "email", label: "EMAIL", type: "email", autoComplete: "email", placeholder: "you@example.com" },
        { name: "password", label: "PASSWORD", type: "password", autoComplete: "new-password", placeholder: "••••••••" },
        { name: "confirmPassword", label: "CONFIRM PASSWORD", type: "password", autoComplete: "new-password", placeholder: "••••••••" },
      ]
    : [
        { name: "email", label: "EMAIL", type: "email", autoComplete: "email", placeholder: "you@example.com" },
        { name: "password", label: "PASSWORD", type: "password", autoComplete: "current-password", placeholder: "••••••••" },
      ];

  return (
    <form onSubmit={submit} className="mt-12">
      <div>
        {fields.map((field) => (
          <label key={field.label} className="block border-b border-[var(--rule)] py-5">
            <span className="meta block">{field.label}</span>
            <input required name={field.name} type={field.type} autoComplete={field.autoComplete} placeholder={field.placeholder} className="mt-3 w-full bg-transparent text-[14px] outline-none placeholder:text-[var(--muted)]" />
          </label>
        ))}
      </div>
      {error ? <p role="alert" className="mt-5 border-l-2 border-[var(--ink)] pl-4 leading-5">{error}</p> : null}
      <div className="mt-8 grid grid-cols-2 gap-3">
        <button disabled={pending} className="h-11 bg-[var(--ink)] px-4 text-[var(--paper)] disabled:opacity-60">{pending ? "Opening…" : create ? "Create account" : "Sign in"}</button>
        <Link href={`${create ? "/sign-in" : "/create-account"}${next ? `?next=${encodeURIComponent(next)}` : ""}`} className="flex h-11 items-center justify-center border border-[var(--ink)] px-4">{create ? "Sign in" : "Create account"}</Link>
      </div>
      {!create ? <Link href="#" className="rule-link mt-7 inline-block text-[var(--muted)]">Forgot password? ↗</Link> : null}
    </form>
  );
}
