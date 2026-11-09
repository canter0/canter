type VisibilitySource = Pick<Document, "visibilityState" | "addEventListener" | "removeEventListener">;

/** Release long polls while hidden. Each visible visit gets its own abort fence;
 * a late response from an earlier visit must not update the current screen. */
export function visibleSubscription(
  source: VisibilitySource,
  follow: (signal: AbortSignal) => Promise<void>,
  onError: (error: unknown) => void,
  retryMs = 3000,
) {
  let disposed = false;
  let active: AbortController | null = null;
  let retry: ReturnType<typeof setTimeout> | undefined;

  function stop() {
    clearTimeout(retry);
    retry = undefined;
    active?.abort();
    active = null;
  }
  function start() {
    if (disposed || source.visibilityState !== "visible" || active) return;
    clearTimeout(retry);
    retry = undefined;
    const controller = new AbortController();
    active = controller;
    void Promise.resolve().then(() => {
      if (!controller.signal.aborted) return follow(controller.signal);
    }).catch(error => {
      if (!controller.signal.aborted) onError(error);
    }).finally(() => {
      if (active !== controller) return;
      active = null;
      if (!disposed && source.visibilityState === "visible") retry = setTimeout(start, retryMs);
    });
  }
  function changed() {
    if (source.visibilityState === "visible") start();
    else stop();
  }
  source.addEventListener("visibilitychange", changed);
  start();
  return () => {
    disposed = true;
    source.removeEventListener("visibilitychange", changed);
    stop();
  };
}
