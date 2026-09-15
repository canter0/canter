import type { Metadata } from "next";
import Link from "next/link";
import { SiteHeader } from "@/components/site-header";
import { PricingPlans } from "@/components/pricing-plans";
import styles from "./pricing.module.css";

export const metadata: Metadata = { title: "Pricing", description: "Start with pay-as-you-go hosting, or put $20 a month toward usage. Bring your agent. Review your costs. Stay in control." };

const questions = [
  { question: "How does the $20 usage credit work?", answer: "Pro costs $20 per month and includes $20 of usage credit. If you use $21, you pay $1 in additional usage: $21 total for that month. The credit resets each billing month and does not roll over." },
  { question: "Is pay as you go free?", answer: "There is no monthly subscription fee. You pay for the compute, object storage and transfer your apps use or reserve." },
  { question: "What will my app cost?", answer: "Your bill follows the resource prices below. Compute starts at $3 per month for 1 vCPU and 1 GB of RAM. Object storage costs $0.014/GB per month, with free reads, writes and direct downloads." },
  { question: "Is my coding agent included?", answer: "Bring your own coding agent. Your agent or model provider bills you separately from Canter hosting." },
];

const comparisons = [
  ["Monthly subscription", "$0 pay as you go · $20 Pro", "$5 Hobby · $20 Pro", "$0"],
  ["Monthly usage credit", "$20 with Pro", "$5 Hobby · $20 Pro", "None, before promotional credits"],
  ["Compute & memory", "$3/month · 1 vCPU + 1 GB RAM", "$20/vCPU + $10/GB RAM per month", "$11.73/month · t3.micro + boot disk + IPv4¹"],
  ["Object storage", "$0.014/GB per month", "$0.015/GB per month · Buckets", "$0.023/GB per month · S3 Standard"],
  ["Object writes", "$0 · included", "$0 · included with Buckets", "$0.005 per 1,000 requests"],
  ["Object reads", "$0 · included", "$0 · included with Buckets", "$0.0004 per 1,000 requests"],
  ["Object downloads", "$0 · direct object egress", "$0 · direct bucket egress", "$0.09/GB after the shared 100 GB allowance²"],
];

export default function PricingPage() {
  return <div className={styles.page}><div className={styles.frame}>
    <SiteHeader />
    <main className={styles.main}>
      <section className={styles.hero} aria-labelledby="pricing-title">
        <span className={styles.eyebrow}><span /> Pricing</span>
        <h1 id="pricing-title">A little to start.<br /><span>Room to keep going.</span></h1>
        <p>Pay for what you run. Put $20 toward usage if you prefer.<br className={styles.desktopBreak} /> Your agent handles the setup. You stay in control.</p>
      </section>
      <PricingPlans />
      <section className={styles.faq} aria-labelledby="questions-title">
        <h2 id="questions-title">Questions<br /> about pricing</h2>
        <div className={styles.questions}>{questions.map((item, index) => <details key={item.question} open={index === 0}>
          <summary>{item.question}<span aria-hidden="true" /></summary><p>{item.answer}</p>
        </details>)}</div>
      </section>
      <section id="compare" className={styles.compare} aria-labelledby="compare-title">
        <span className={styles.eyebrow}><span /> Cost comparison</span>
        <h2 id="compare-title">Compare hosting costs.</h2>
        <p className={styles.sectionIntro}>Compute from $3/month. Object storage at $0.014/GB. Free reads, writes and downloads.</p>
        <div className={styles.tableScroll} tabIndex={0} role="region" aria-label="Hosting pricing comparison, scroll horizontally on small screens">
          <table><caption className={styles.srOnly}>Hosting costs for Canter, Railway and AWS EC2 with S3</caption><thead><tr><th scope="col">Cost</th><th scope="col" className={styles.canterColumn}>Canter</th><th scope="col">Railway <small>Compute + Buckets</small></th><th scope="col">AWS <small>EC2 + S3 Standard</small></th></tr></thead>
            <tbody>{comparisons.map(([label, ...values]) => <tr key={label}><th scope="row">{label}</th>{values.map((value, index) => <td className={index === 0 ? styles.canterColumn : undefined} key={index}>{value}</td>)}</tr>)}</tbody>
          </table>
        </div>
        <div className={styles.comparisonNotes}>
          <p>USD, before tax. Monthly compute figures use 720 hours. Storage rows compare object storage. Competitor prices checked September 15, 2026.</p>
          <p>¹ Canter compute includes a public IPv4 address and local disk; memory includes system overhead. AWS uses Linux on-demand t3.micro in US East (N. Virginia), with 2 burstable vCPUs, 1 GiB RAM, an 8 GB boot disk and one public IPv4. CPU performance differs. Railway bills actual CPU and RAM consumption; reserved capacity and measured consumption produce different bills.</p>
          <p>² S3 Standard rates shown for US East (N. Virginia), with the shared AWS 100 GB monthly transfer allowance. Direct object downloads are separate from app traffic. Promotional credits and commitment discounts are excluded.</p>
          <div className={styles.sources}><span>Comparison sources</span><a href="https://docs.railway.com/pricing/plans" target="_blank" rel="noreferrer">Railway compute ↗</a><a href="https://docs.railway.com/storage-buckets/billing" target="_blank" rel="noreferrer">Railway Buckets ↗</a><a href="https://aws.amazon.com/ec2/instance-types/t3/" target="_blank" rel="noreferrer">EC2 ↗</a><a href="https://aws.amazon.com/s3/pricing/" target="_blank" rel="noreferrer">S3 ↗</a><a href="https://aws.amazon.com/ebs/pricing/" target="_blank" rel="noreferrer">EC2 boot disk ↗</a><a href="https://aws.amazon.com/vpc/pricing/" target="_blank" rel="noreferrer">IPv4 ↗</a></div>
        </div>
      </section>
      <section className={styles.bottomCTA}><div><span className={styles.eyebrow}><span /> Bring your agent</span><h2>Your next app starts here.</h2><p>Connect your agent. Review the plan. Let Canter run it.</p></div><Link href="/create-account">Get started <span aria-hidden="true">↗</span></Link></section>
    </main>
    <footer className={styles.footer}><Link href="/" className="wordmark">canter</Link><span>A place for your agents to run.</span><nav aria-label="Footer"><a href="https://github.com/canter0/canter#how-it-works">Docs</a><a href="https://github.com/canter0/canter">GitHub</a><Link href="/sign-in">Sign in</Link></nav></footer>
  </div></div>;
}
