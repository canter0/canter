import Link from "next/link";
import { AgentOnboardingPrompt } from "./agent-onboarding-prompt";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./landing-closing.module.css";

export function LandingGetStarted() {
  return (
    <section className={styles.section} id="get-started" aria-labelledby="get-started-title">
      <div className={styles.content}>
        <h2 id="get-started-title">Get started with Canter.</h2>
        <AgentOnboardingPrompt className={styles.onboarding} />
        <Link className={styles.account} href="/create-account">
          Create an account <WorkspaceIcon name="external" width={17} height={17} />
        </Link>
      </div>
    </section>
  );
}

export function LandingFooter() {
  return (
    <footer className={styles.footer}>
      <div className={styles.footerInner}>
        <Link className={`wordmark ${styles.wordmark}`} href="/" aria-label="Canter home">canter</Link>
        <nav aria-label="Product">
          <h2>Product</h2>
          <Link href="/#workload-vps">VPS</Link>
          <Link href="/#workload-sandboxes">Sandboxes</Link>
          <Link href="/#workload-ci">CI</Link>
          <Link href="/pricing">Pricing</Link>
        </nav>
        <nav aria-label="Social">
          <h2>Social</h2>
          <a href="https://discord.gg/JdnuhEEe7G">Discord</a>
          <a href="https://x.com/canterdev">X</a>
        </nav>
        <nav aria-label="Legal">
          <h2>Legal</h2>
          <a href="/terms">Terms of Service</a>
          <a href="/privacy">Privacy Policy</a>
        </nav>
      </div>
    </footer>
  );
}
