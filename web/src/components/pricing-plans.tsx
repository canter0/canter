import Link from "next/link";
import { dollars, planStartURL, pricingCatalog, type PlanID } from "@/lib/pricing";
import styles from "@/app/pricing/pricing.module.css";

export function PricingPlans() {
  return <section className={styles.plans} aria-label="Pricing plans">
    <div className={styles.planGrid}>{pricingCatalog.plans.map((plan) => {
      const id = plan.id as PlanID;
      const isPro = id === "pro";
      return <article key={id} className={isPro ? styles.proCard : styles.planCard} aria-labelledby={id + "-title"}>
        <div className={styles.planName}><h2 id={id + "-title"}>{plan.name}</h2>{isPro ? <span>Usage included</span> : null}</div>
        <div className={styles.planPrice}>{dollars(plan.monthlyCents)}<span>/ month</span></div>
        <p className={styles.planDescription}>{isPro ? "Includes $20 of usage each month. Only pay extra for usage above $20." : "No monthly subscription fee. Just pay for what your apps use."}</p>
        <Link href={planStartURL(id)} className={isPro ? styles.primaryButton : styles.secondaryButton}>{isPro ? "Start with Pro" : "Start with usage"}<span aria-hidden="true">↗</span></Link>
        <ul className={styles.planFacts}><li>{isPro ? "$20 of usage credit each month" : "No monthly subscription fee"}</li><li>{isPro ? "Only pay extra above your credit" : "Usage billed monthly"}</li><li>Bring your own coding agent</li><li>Review changes before they run</li></ul>
      </article>;
    })}</div>
    <p className={styles.finePrint}>All prices in USD, before tax.</p>
  </section>;
}
