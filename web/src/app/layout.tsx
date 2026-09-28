import type { Metadata } from "next";
import type { ReactNode } from "react";
import "./globals.css";
import { CanterSiteTools } from "@/components/canter-site-tools";
import { siteDescription, siteOrigin, siteTitle } from "@/lib/seo";

export const metadata: Metadata = {
  metadataBase: new URL(siteOrigin),
  title: {
    default: siteTitle,
    template: "%s — Canter",
  },
  description: siteDescription,
  // Public pages opt in individually. Account, authorization, approval and
  // workspace routes inherit noindex without exposing private URLs in robots.txt.
  robots: { index: false, follow: true },
};

export default function RootLayout({ children }: { children: ReactNode }) {
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
