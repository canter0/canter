import Link from "next/link";
import { AgentOnboardingPrompt } from "@/components/agent-onboarding-prompt";
import { DitherBackground } from "@/components/dither-background";
import { SiteHeader } from "@/components/site-header";
import styles from "./home.module.css";

export default function Home() {
  return (
    <div className={styles.landing}>
      <div className={styles.frame}>
        <DitherBackground className={styles.background} />
        <SiteHeader />
        <main className={styles.hero}>
          <h1 className={styles.headline}>
            <span>A place for your</span>
            <span className={styles.accent}>agents to run.</span>
          </h1>
          <p className={styles.description}>
            Your agent builds it. Canter runs it.
            <br />
            You stay in control.
          </p>
          <div className={styles.start}>
            <Link href="/create-account" className={styles.startButton}>Get started with Canter <span aria-hidden="true">↗</span></Link>
          </div>
          <AgentOnboardingPrompt />
        </main>
      </div>
    </div>
  );
}
