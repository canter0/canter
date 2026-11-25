import type { ReactNode } from "react";
import { AcquisitionVisit } from "@/components/acquisition-visit";
import { LandingFooter } from "@/components/landing-closing";
import { SiteHeader } from "@/components/site-header";
import home from "@/app/home.module.css";
import styles from "./pricing.module.css";

export default function PricingLayout({ children }: { children: ReactNode }) {
  return (
    <div className={home.landing}>
      <AcquisitionVisit landingPath="/pricing" />
      <div className={home.frame}>
        <SiteHeader sticky />
        <main className={styles.main}>{children}</main>
        <LandingFooter />
      </div>
    </div>
  );
}
