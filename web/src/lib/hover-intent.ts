export type PointerPoint = { x: number; y: number };
export type HoverRect = { left: number; right: number; top: number; bottom: number };

export function pointInHoverRect(point: PointerPoint, rect: HoverRect, buffer = 0) {
  return point.x >= rect.left - buffer && point.x <= rect.right + buffer && point.y >= rect.top - buffer && point.y <= rect.bottom + buffer;
}

// Join the row to the full card, including paths entering its top or bottom.
// A triangle ending only at the near edge cuts off those paths when a tall menu
// places the card below the row. Vertical browsing and reversing still release
// the corridor immediately, with a small allowance for pointer jitter.
export function movingThroughHoverTriangle(point: PointerPoint, previous: PointerPoint, origin: PointerPoint, card: HoverRect, side: "left" | "right") {
  const direction = side === "right" ? 1 : -1;
  if ((point.x - previous.x) * direction < -2 || (point.x - origin.x) * direction <= 0) return false;
  const buffer = 6;
  const near = (side === "right" ? card.left : -card.right) - buffer;
  const far = (side === "right" ? card.right : -card.left) + buffer;
  const start = origin.x * direction - buffer;
  const x = point.x * direction;
  if (near <= start || x < start || x > far) return false;
  const topEdge = origin.y < card.top ? far : near;
  const bottomEdge = origin.y > card.bottom ? far : near;
  const top = origin.y - buffer + (card.top - origin.y) * Math.min(1, (x - start) / (topEdge - start));
  const bottom = origin.y + buffer + (card.bottom - origin.y) * Math.min(1, (x - start) / (bottomEdge - start));
  return point.y >= top && point.y <= bottom;
}
