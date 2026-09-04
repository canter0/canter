import Link from "next/link";
import type { ReactNode } from "react";

export function AuthShell({ children }: { children: ReactNode }) {
  return (
    <main className="min-h-screen px-6 sm:px-10 lg:px-16">
      <header className="flex h-24 items-center">
        <Link href="/" className="wordmark text-[24px] leading-none">canter</Link>
      </header>
      <div className="mx-auto flex min-h-[calc(100vh-96px)] w-full max-w-[480px] items-center pb-24">
        <div className="w-full">{children}</div>
      </div>
    </main>
  );
}

export function AuthTitle({ eyebrow, title, description }: { eyebrow?: string; title: string; description?: string }) {
  return (
    <div>
      <span className="signal mb-7" />
      {eyebrow ? <div className="meta mb-3">{eyebrow}</div> : null}
      <h1 className="display text-[42px] leading-none tracking-[-0.035em]">{title}</h1>
      {description ? <p className="display mt-5 max-w-[460px] text-[17px] leading-[1.5] text-[var(--muted)]">{description}</p> : null}
    </div>
  );
}
