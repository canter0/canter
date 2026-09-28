import type { Metadata } from "next";

export const siteOrigin = "https://canter.dev";
export const siteTitle = "Canter — App Hosting You Can Manage Through Chat";
export const siteDescription = "Deploy and manage your apps through Canter or your own coding agent. Review the deployment plan and hosting cost, approve changes, and see what is running.";

export function publicPageMetadata(path: string, title: string, description: string): Metadata {
  const url = new URL(path, siteOrigin).href;
  return {
    title: { absolute: title },
    description,
    alternates: { canonical: url },
    robots: { index: true, follow: true },
    openGraph: {
      type: "website", siteName: "Canter", title, description, url,
      images: [{ url: `${siteOrigin}/og.png`, width: 1200, height: 630, alt: "Canter — Hosting, operated by your agent." }],
    },
    twitter: { card: "summary_large_image", title, description, images: [`${siteOrigin}/og.png`] },
  };
}
