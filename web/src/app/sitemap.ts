import type { MetadataRoute } from "next";
import { publicProducts } from "@/lib/public-products";
import { siteOrigin } from "@/lib/seo";

export default function sitemap(): MetadataRoute.Sitemap {
  const productPaths = Object.values(publicProducts).map(product => product.path);
  return ["/", "/pricing", "/pricing/infra", "/terms", "/privacy", ...productPaths].map(path => ({ url: `${siteOrigin}${path}` }));
}
