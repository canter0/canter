import Link from "next/link";
import type { ReactNode } from "react";

export function LoginShell({ create = false, children }: { create?: boolean; children: ReactNode }) {
  return (
    <main className="flex min-h-svh items-center justify-center bg-[#131313] px-6 py-16 text-[#e5e5e5] [color-scheme:dark] [font-family:Arial,sans-serif]">
      <div className="w-full max-w-[384px]">
        <div className="mb-7 text-center">
          <Link href="/" aria-label="Canter home" className="wordmark mb-9 inline-block text-[38px] leading-none tracking-[-0.04em]">canter</Link>
          <h1 className="text-[20px] font-medium tracking-[-0.025em]">{create ? "Welcome to Canter" : "Welcome back"}</h1>
          <p className="mt-2 text-[14px] text-[#888]">{create ? "Create a new account" : "Log in to your account"}</p>
        </div>
        {children}
      </div>
    </main>
  );
}
