import Link from "next/link";
import { AgentOnboardingPrompt } from "@/components/agent-onboarding-prompt";
import { DitherBackground } from "@/components/dither-background";
import { SiteHeader } from "@/components/site-header";
import { AcquisitionVisit } from "@/components/acquisition-visit";
import { publicPageMetadata, siteDescription, siteTitle } from "@/lib/seo";
import styles from "./home.module.css";

export const metadata = publicPageMetadata("/", siteTitle, siteDescription);

export default function Home() {
  return (
    <div className={styles.landing}>
      <AcquisitionVisit landingPath="/" />
      <div className={styles.frame}>
        <DitherBackground className={styles.background} />
        <SiteHeader />
        <main className={styles.hero}>
          <h1 className={styles.headline}>
            <span>Infrastructure</span>
            <span className={styles.accent}>you can talk to.</span>
          </h1>
          <p className={styles.description}>
            <span>Deploy and manage your apps through Canter or your own agent.</span>
            {" "}<span>Review the plan, approve changes, and see what’s running.</span>
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
