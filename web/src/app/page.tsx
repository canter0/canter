import Link from "next/link";
import { AgentCarousel } from "@/components/agent-carousel";
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
        <main>
          <section className={styles.hero} aria-labelledby="home-headline">
            <DitherBackground className={styles.background} />
            <h1 id="home-headline" className={styles.headline}>
              <span data-motion="reveal">Infrastructure</span>
              <span className={styles.accent} data-motion="reveal" data-motion-delay="70">you can talk to.</span>
            </h1>
            <p className={styles.description} data-motion="reveal" data-motion-delay="120">
              <span>Deploy and manage your apps through Canter or your own agent.</span>
              {" "}<span>Review the plan, approve changes, and see what’s running.</span>
            </p>
            <div className={styles.start} data-motion="reveal" data-motion-delay="170">
              <Link href="/create-account" className={styles.startButton}>
                Get started with Canter
                <svg className={styles.startArrow} width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                  <path d="M6 18 18 6M6 6h12v12" />
                </svg>
              </Link>
            </div>
            <AgentOnboardingPrompt />
          </section>
          <AgentCarousel />
        </main>
      </div>
    </div>
  );
}
