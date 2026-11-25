import { PublicProductPage, productMetadata } from "@/components/public-product-page";

export const metadata = productMetadata("super-sandboxes");

export default function Page() {
  return <PublicProductPage id="super-sandboxes" />;
}
