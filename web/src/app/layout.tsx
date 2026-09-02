import type { Metadata } from "next";
import "./globals.css";
import { CanterSiteTools } from "@/components/canter-site-tools";

export const metadata: Metadata = {
  metadataBase: new URL("https://canter.dev"),
  title: {
    default: "Canter — Give agents somewhere to run.",
    template: "%s — Canter",
  },
  description:
    "An agent-operated, human-governed hosting control plane built around inspectable Changes.",
  openGraph: {
    title: "Canter — Give agents somewhere to run.",
    description: "Your agent proposes infrastructure Changes. Canter governs execution, approval, rollback, and proof.",
  },
  twitter: {
    card: "summary",
    title: "Canter — Give agents somewhere to run.",
    description: "Your agent proposes infrastructure Changes. Canter governs execution, approval, rollback, and proof.",
  },
};

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html lang="en">
      <head>
        <link rel="alternate" type="application/json" href="/.well-known/canter" title="Canter agent discovery" />
        <link rel="alternate" type="text/plain" href="/llms.txt" title="Canter agent instructions" />
      </head>
      <body><CanterSiteTools />{children}</body>
    </html>
  );
}
