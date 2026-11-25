import { PublicProductPage, productMetadata } from "@/components/public-product-page";

export const metadata = productMetadata("ci");

export default function Page() {
  return <PublicProductPage id="ci" />;
}
