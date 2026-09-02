import { SiteHeader } from "@/components/site-header";

export default function Home() {
  return (
    <main className="flex min-h-screen flex-col">
      <SiteHeader />
      <section className="flex flex-1 items-end px-6 pb-12 sm:px-10 sm:pb-16 lg:px-16 lg:pb-20">
        <h1 className="display text-[clamp(58px,8.75vw,126px)] leading-[0.86] tracking-[-0.055em]">
          <span className="block">Give agents</span>
          <span className="block">somewhere to run.</span>
        </h1>
      </section>
    </main>
  );
}
