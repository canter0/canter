export type PointerPoint = { x: number; y: number };
export type HoverRect = { left: number; right: number; top: number; bottom: number };

export function pointInHoverRect(point: PointerPoint, rect: HoverRect, buffer = 0) {
  return point.x >= rect.left - buffer && point.x <= rect.right + buffer && point.y >= rect.top - buffer && point.y <= rect.bottom + buffer;
}

// A safe triangle connects the last point on the active row to the near edge of
// its card. Direction matters: vertical list browsing and reversing course must
// be immediate. See https://floating-ui.com/docs/usehover#safepolygon.
export function movingThroughHoverTriangle(point: PointerPoint, previous: PointerPoint, origin: PointerPoint, card: HoverRect, side: "left" | "right") {
  const direction = side === "right" ? 1 : -1;
  if ((point.x - previous.x) * direction <= 0) return false;
  const edge = side === "right" ? card.left + 6 : card.right - 6;
  const startX = origin.x - direction * 6;
  const progress = (point.x - startX) / (edge - startX);
  if (progress < 0 || progress > 1) return false;
  // Buffer both ends so the corridor includes its origin, even when the card
  // sits entirely below or above the row. This also tolerates pointer jitter.
  const top = origin.y - 6 + (card.top - origin.y) * progress;
  const bottom = origin.y + 6 + (card.bottom - origin.y) * progress;
  return point.y >= top && point.y <= bottom;
}
