import { WorkspaceIcon, type WorkspaceIconName } from "@/components/workspace-icon";
import { infraRates } from "@/lib/pricing-estimate";
import { publicPageMetadata } from "@/lib/seo";
import { PricingHeading, PricingQuestions } from "../pricing-content";
import styles from "../pricing.module.css";
import { PricingCalculator } from "./pricing-calculator";

export const metadata = publicPageMetadata("/pricing/infra", "Infrastructure Pricing — Canter", "See Canter’s per-minute rates for Sandboxes, VPS, and CI, plus object storage pricing. Estimate your workload cost.");

const rates = [
  { label: "Sandboxes", icon: "sandboxTerminal", rate: infraRates.sandboxes },
  { label: "VPS", icon: "serverStack", rate: infraRates.vps },
  { label: "CI", icon: "workflowCheck", rate: infraRates.ci },
  { label: "Object storage", icon: "stack", rate: infraRates.objects },
] satisfies { label: string; icon: WorkspaceIconName; rate: string }[];

export default function InfrastructurePricingPage() {
  return (
    <>
      <PricingHeading active="infra" title="Infrastructure pricing" description="See what each resource costs." />
      <section className={styles.section} aria-labelledby="rates-title">
        <h2 id="rates-title">Resource rates</h2>
        <div className={styles.tableFrame}>
          <table className={styles.table}>
            <thead><tr><th scope="col">Resource</th><th scope="col">Billing rate</th></tr></thead>
            <tbody>
              {rates.map(({ label, icon, rate }) => (
                <tr key={label}>
                  <th scope="row"><span className={styles.resourceLabel}><WorkspaceIcon name={icon} width={22} height={22} />{label}</span></th>
                  <td>{rate}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
      <PricingCalculator />
      <PricingQuestions infra />
    </>
  );
}
