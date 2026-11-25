import { PublicProductPage, productMetadata } from "@/components/public-product-page";

export const metadata = productMetadata("apps");

export default function Page() {
  return <PublicProductPage id="apps" />;
}
