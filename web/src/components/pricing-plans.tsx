import Link from "next/link";
import { dollars, planStartURL, pricingCatalog, type PlanID } from "@/lib/pricing";
import styles from "@/app/pricing/pricing.module.css";

export function PricingPlans() {
  return (
    <section className={styles.planGrid} aria-label="Pricing plans">
      {pricingCatalog.plans.map(plan => {
        const id = plan.id as PlanID;
        const isPro = id === "pro";
        return (
          <article key={id} className={styles.planCard} aria-labelledby={id + "-title"}>
            <h2 id={id + "-title"}>{plan.name}</h2>
            <p className={styles.planPrice}>{dollars(plan.monthlyCents)} <span>/ month</span></p>
            <p className={styles.planDescription}>
              {isPro ? `Includes ${dollars(plan.includedUsageCents)} of eligible usage each month.` : "No subscription fee. Pay for the resources you use."}
            </p>
            <Link href={planStartURL(id)} className={isPro ? styles.primaryButton : styles.secondaryButton}>
              {isPro ? "Start with Pro" : "Start with usage"}
            </Link>
          </article>
        );
      })}
    </section>
  );
}
