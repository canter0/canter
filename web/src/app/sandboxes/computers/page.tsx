import { PublicProductPage, productMetadata } from "@/components/public-product-page";

export const metadata = productMetadata("computers");

export default function Page() {
  return <PublicProductPage id="computers" />;
}
