import Link from "next/link";
import { WorkspaceIcon } from "@/components/workspace-icon";
import styles from "./pricing.module.css";

export function PricingHeading({ active, title, description }: { active: "subscriptions" | "infra"; title: string; description: string }) {
  return (
    <div className={styles.heading}>
      <nav className={styles.switcher} aria-label="Pricing pages">
        <Link aria-current={active === "subscriptions" ? "page" : undefined} href="/pricing">Subscriptions</Link>
        <Link aria-current={active === "infra" ? "page" : undefined} href="/pricing/infra">Infra</Link>
      </nav>
      <h1>{title}</h1>
      <p>{description}</p>
    </div>
  );
}

export function PricingQuestions({ infra = false }: { infra?: boolean }) {
  return (
    <details className={styles.questions}>
      <summary>Billing questions <WorkspaceIcon name="down" width={18} height={18} /></summary>
      <div className={styles.answers}>
        {infra ? <>
          <h3>What does the estimate cover?</h3>
          <p>The calculator estimates one VPS, sandbox awake time, or paid CI runtime for the selected size. Plan credits, storage, transfer, and tax are separate.</p>
          <h3>What do object storage requests cost?</h3>
          <p>Object storage charges for data stored. Reads, writes, and direct object downloads are included.</p>
          <Link className={styles.textLink} href="/#compare-costs">Compare infrastructure costs <WorkspaceIcon name="external" width={16} height={16} /></Link>
        </> : <>
          <h3>When do I add a payment method?</h3>
          <p>Every organization starts on pay as you go. The owner adds a card through Stripe before deploying paid resources or subscribing to Pro.</p>
          <h3>Is pay as you go free?</h3>
          <p>There is no monthly subscription fee. You pay for the resources your apps use or reserve.</p>
          <h3>Is my coding agent included?</h3>
          <p>Bring your own coding agent. Your agent or model provider bills you separately from Canter hosting.</p>
        </>}
      </div>
    </details>
  );
}
