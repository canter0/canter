export const motion = {
  ease: "cubic-bezier(0.22, 1, 0.36, 1)",
  text: 280,
  reveal: 420,
  panel: 280,
  exit: 140,
  layout: 320,
  press: 90,
} as const;

/** Cancel even an in-flight animation when the system motion preference changes. */
export function playMotion(element: HTMLElement, keyframes: Keyframe[], options: KeyframeAnimationOptions = {}) {
  const preference = window.matchMedia("(prefers-reduced-motion: reduce)");
  if (preference.matches) return null;
  const animation = element.animate(keyframes, { duration: motion.panel, easing: motion.ease, fill: "backwards", ...options });
  const changed = () => { if (preference.matches) animation.cancel(); };
  const cleanup = () => {
    preference.removeEventListener("change", changed);
    animation.removeEventListener("finish", cleanup);
    animation.removeEventListener("cancel", cleanup);
  };
  preference.addEventListener("change", changed);
  animation.addEventListener("finish", cleanup);
  animation.addEventListener("cancel", cleanup);
  return animation;
}
