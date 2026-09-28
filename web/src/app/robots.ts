import type { MetadataRoute } from "next";
import { siteOrigin } from "@/lib/seo";

export default function robots(): MetadataRoute.Robots {
  // Crawling must remain allowed so search engines can read route-level noindex.
  return { rules: { userAgent: "*", allow: "/" }, sitemap: `${siteOrigin}/sitemap.xml` };
}
