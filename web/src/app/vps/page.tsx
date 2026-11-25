import { PublicProductPage, productMetadata } from "@/components/public-product-page";

export const metadata = productMetadata("vps");

export default function Page() {
  return <PublicProductPage id="vps" />;
}
