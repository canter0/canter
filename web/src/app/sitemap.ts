import type { MetadataRoute } from "next";
import { siteOrigin } from "@/lib/seo";

export default function sitemap(): MetadataRoute.Sitemap {
  return [{ url: `${siteOrigin}/` }, { url: `${siteOrigin}/pricing` }];
}
