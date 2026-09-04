import Link from "next/link";

export function SiteHeader() {
  return (
    <header className="flex h-24 items-center justify-between px-6 sm:px-10 lg:px-16">
      <Link href="/" className="wordmark text-[24px] leading-none" aria-label="Canter home">canter</Link>
      <Link href="/sign-in" className="rule-link text-[11px]">Sign in ↗</Link>
    </header>
  );
}
