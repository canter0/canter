import Link from "next/link";
import { PricingPlans } from "@/components/pricing-plans";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { dollars, estimateBill } from "@/lib/pricing";
import { publicPageMetadata } from "@/lib/seo";
import { PricingHeading, PricingQuestions } from "./pricing-content";
import styles from "./pricing.module.css";

export const metadata = publicPageMetadata("/pricing", "Pricing — Canter", "Compare Canter’s pay-as-you-go and Pro plans and see how the monthly usage credit affects your bill.");

export default function PricingPage() {
  return (
    <>
      <PricingHeading active="subscriptions" title="Pricing" description="Choose how you pay for Canter." />
      <PricingPlans />
      <section className={styles.section} aria-labelledby="credit-title">
        <h2 id="credit-title">How the credit works</h2>
        <div className={styles.tableFrame}>
          <table className={styles.table}>
            <thead><tr><th scope="col">Eligible usage</th><th scope="col">Pay as you go</th><th scope="col">Pro</th></tr></thead>
            <tbody>
              {[800, 3000].map(usage => (
                <tr key={usage}>
                  <th scope="row">{dollars(usage)}</th>
                  <td>{dollars(estimateBill("payg", usage).totalCents)}</td>
                  <td>{dollars(estimateBill("pro", usage).totalCents)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <p className={styles.note}>Illustrative monthly bills in USD, before tax.</p>
        <Link id="compare" className={styles.textLink} href="/pricing/infra">
          View infrastructure rates <WorkspaceIcon name="external" width={16} height={16} />
        </Link>
      </section>
      <PricingQuestions />
    </>
  );
}
