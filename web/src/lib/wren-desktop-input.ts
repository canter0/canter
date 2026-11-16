import type { WrenDesktopInput } from "./wren-work";

const named: Record<string, string> = {
  Enter: "Return",
  Backspace: "BackSpace",
  Escape: "Escape",
  Tab: "Tab",
  ArrowLeft: "Left",
  ArrowRight: "Right",
  ArrowUp: "Up",
  ArrowDown: "Down",
  Delete: "Delete",
  Home: "Home",
  End: "End",
  PageUp: "Prior",
  PageDown: "Next",
  Insert: "Insert",
  " ": "space",
};
export function desktopKey(event: {
  key: string;
  ctrlKey: boolean;
  metaKey: boolean;
  altKey: boolean;
  shiftKey: boolean;
  isComposing?: boolean;
}): WrenDesktopInput | null {
  if (
    event.isComposing ||
    [
      "Shift",
      "Control",
      "Alt",
      "Meta",
      "Dead",
      "Unidentified",
      "Process",
    ].includes(event.key)
  )
    return null;
  if (
    !event.ctrlKey &&
    !event.metaKey &&
    !event.altKey &&
    event.key.length === 1
  )
    return { kind: "type", text: event.key };
  const key =
    named[event.key] ??
    (/^F([1-9]|1[0-2])$/.test(event.key) || /^[a-z0-9]$/i.test(event.key)
      ? event.key
      : undefined);
  if (!key) return null;
  // The guest is Linux. Familiar Command shortcuts address its Control key.
  return {
    kind: "key",
    key: [
      event.ctrlKey || event.metaKey ? "ctrl" : "",
      event.altKey ? "alt" : "",
      event.shiftKey ? "shift" : "",
      key,
    ]
      .filter(Boolean)
      .join("+"),
  };
}

// Input is ordered and bounded. Adjacent typing coalesces while an earlier
// action is in flight; clicks/key chords form boundaries. A failed mutation
// cancels everything behind it instead of replaying into an unknown UI state.
export function desktopInputQueue(
  send: (input: WrenDesktopInput) => Promise<boolean>,
  changed?: (pending: number) => void,
) {
  let queue: WrenDesktopInput[] = [],
    running = false,
    disposed = false,
    generation = 0;
  async function drain() {
    if (running || disposed) return;
    running = true;
    const token = generation;
    try {
      while (queue.length && !disposed && token === generation) {
        const next = queue.shift()!;
        changed?.(queue.length + 1);
        if (!(await send(next))) {
          queue = [];
          break;
        }
      }
    } finally {
      running = false;
      changed?.(queue.length);
      if (queue.length && !disposed) void drain();
    }
  }
  return {
    setSender(next: typeof send) {
      send = next;
    },
    push(input: WrenDesktopInput) {
      if (disposed || queue.length >= 80) return false;
      const last = queue.at(-1);
      if (
        input.kind === "type" &&
        last?.kind === "type" &&
        (last.text?.length ?? 0) + (input.text?.length ?? 0) < 8000
      )
        last.text = (last.text ?? "") + (input.text ?? "");
      else if (input.kind === "move" && last?.kind === "move")
        queue[queue.length - 1] = input;
      else queue.push(input);
      changed?.(queue.length + (running ? 1 : 0));
      void drain();
      return true;
    },
    clear() {
      queue = [];
      generation++;
      changed?.(running ? 1 : 0);
    },
    dispose() {
      disposed = true;
      queue = [];
      generation++;
    },
  };
}
