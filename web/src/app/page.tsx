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
        <SiteHeader />
        <main className={styles.hero}>
          <DitherBackground className={styles.background} />
          <h1 className={styles.headline}>
            <span>Infrastructure</span>
            <span className={styles.accent}>you can talk to.</span>
          </h1>
          <p className={styles.description}>
            <span>Deploy and manage your apps through Canter or your own agent.</span>
            {" "}<span>Review the plan, approve changes, and see what’s running.</span>
          </p>
          <div className={styles.start}>
            <Link href="/create-account" className={styles.startButton}>
              Get started with Canter
              <svg className={styles.startArrow} width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                <path d="M6 18 18 6M6 6h12v12" />
              </svg>
            </Link>
          </div>
          <AgentOnboardingPrompt />
        </main>
      </div>
    </div>
  );
}
