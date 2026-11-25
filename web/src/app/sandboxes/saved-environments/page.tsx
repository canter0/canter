import { PublicProductPage, productMetadata } from "@/components/public-product-page";

export const metadata = productMetadata("saved-environments");

export default function Page() {
  return <PublicProductPage id="saved-environments" />;
}
