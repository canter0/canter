import type { OperatorSurface } from "./operator-api";

/** Match the container to the task, rather than treating every object as a panel. */
export function wrenSurfacePlacement(kind: OperatorSurface["kind"]) {
  if (kind === "wren-extensions") return "none";
  if (["wren-memory", "wren-skills", "wren-members"].includes(kind)) return "dialog";
  if (["wren-job", "wren-artifact", "wren-library", "wren-projects", "wren-responsibility", "wren-messaging"].includes(kind)) return "work";
  return "split";
}
