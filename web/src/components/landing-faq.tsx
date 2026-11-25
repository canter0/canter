import Link from "next/link";
import { LandingFAQChat } from "./landing-faq-chat";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./landing-faq.module.css";

const questions = [
  {
    question: "Can I deploy a GitHub repo I already have?",
    answer: "Yes. Connect GitHub in your Canter workspace, choose the repository, and review the resources in the deployment plan before approving it.",
    href: "/examples/resources",
    link: "See deployment examples",
  },
  {
    question: "Can I run my app and Postgres together?",
    answer: "Yes. A Canter System can describe your web service, its Postgres database, and a public HTTPS endpoint together. You review the resources as one deployment plan.",
    href: "/#workload-vps",
    link: "Explore app hosting",
  },
  {
    question: "What can my agent change without my approval?",
    answer: "A connected agent can inspect the resources it has access to and draft changes. Canter only applies a change after you approve it or it fits a policy you’ve already set.",
    href: "/llms.txt",
    link: "Read the agent setup guide",
  },
  {
    question: "Do I have to rewrite my GitHub Actions workflows?",
    answer: "You can keep your workflow and point the jobs you want to move at a Canter runner. Check that the runner has the tools and environment those steps expect.",
    href: "/#workload-ci",
    link: "Explore CI runners",
  },
];

export function LandingFAQ() {
  return (
    <section className={styles.section} id="faq" aria-labelledby="faq-title">
      <h2 id="faq-title">Frequently asked questions</h2>
      <div className={styles.questions}>
        {questions.map(({ question, answer, href, link }) => (
          <details key={question}>
            <summary>{question}<WorkspaceIcon name="plus" width={20} height={20} /></summary>
            <div className={styles.expanded}>
              <p>{answer}</p>
              <Link href={href} className={styles.source}>{link}<WorkspaceIcon name="external" width={14} height={14} /></Link>
            </div>
          </details>
        ))}
      </div>
      <LandingFAQChat />
    </section>
  );
}
