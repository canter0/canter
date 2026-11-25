import { publicProducts, type PublicProductId } from "@/lib/public-products";
import { publicPageMetadata } from "@/lib/seo";
import { LandingFooter } from "./landing-closing";
import { productExamples } from "./public-product-examples";
import { SiteHeader } from "./site-header";
import { WorkspaceIcon } from "./workspace-icon";
import home from "@/app/home.module.css";
import styles from "./public-product.module.css";

export function productMetadata(id: PublicProductId) {
  const product = publicProducts[id];
  return publicPageMetadata(product.path, `${product.label} — Canter`, product.hero.description);
}

export function PublicProductPage({ id }: { id: PublicProductId }) {
  const product = publicProducts[id];
  const [heroExample, ...sectionExamples] = productExamples[id];
  const startHref = `/products/start?product=${id}`;

  return (
    <div className={home.landing}>
      <div className={home.frame}>
        <SiteHeader sticky getStartedHref={startHref} />
        <main className={styles.main} data-product={id}>
          <section className={styles.hero} aria-labelledby="product-title">
            <div className={styles.heroInner}>
              <div className={styles.heroCopy}>
                <p className={styles.productLabel}>
                  <WorkspaceIcon name={product.icon} width={20} height={20} />
                  {product.label}
                </p>
                <h1 id="product-title">{product.hero.title}</h1>
                <p className={styles.description}>{product.hero.description}</p>
                <a className={home.startButton} href={startHref}>
                  Ask Canter <WorkspaceIcon name="external" width={20} height={20} />
                </a>
              </div>
              <div className={styles.visual}>{heroExample}</div>
            </div>
          </section>
          {product.sections.map(({ title, description }, index) => (
            <section key={title} className={styles.section} aria-labelledby={`product-section-${index}`}>
              <div className={styles.sectionInner}>
                <div className={styles.sectionCopy}>
                  <h2 id={`product-section-${index}`}>{title}</h2>
                  <p className={styles.description}>{description}</p>
                </div>
                <div className={styles.visual}>{sectionExamples[index]}</div>
              </div>
            </section>
          ))}
        </main>
        <LandingFooter />
      </div>
    </div>
  );
}
