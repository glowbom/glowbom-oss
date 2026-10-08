export function stackMenuPosition(
  anchor: { left: number; top: number; bottom: number },
  viewport: { width: number; height: number; left?: number; top?: number },
  contentHeight: number,
) {
  const edge = 12;
  const gap = 8;
  const viewportLeft = viewport.left || 0;
  const viewportTop = viewport.top || 0;
  const width = Math.min(284, Math.max(0, viewport.width - edge * 2));
  const fullHeight = Math.max(0, viewport.height - edge * 2);
  const below = Math.max(0, viewportTop + viewport.height - anchor.bottom - gap - edge);
  const above = Math.max(0, anchor.top - viewportTop - gap - edge);
  const needed = contentHeight || 320;
  const upward = below < Math.min(needed, 320) && above > below;
  const maxHeight = Math.min(440, fullHeight, Math.max(upward ? above : below, Math.min(160, fullHeight)));
  const height = Math.min(needed, maxHeight);
  const left = Math.min(Math.max(anchor.left, viewportLeft + edge), viewportLeft + viewport.width - edge - width);
  const wantedTop = upward ? anchor.top - gap - height : anchor.bottom + gap;
  const top = Math.min(Math.max(wantedTop, viewportTop + edge), viewportTop + viewport.height - edge - height);
  return { left, top, width, maxHeight };
}
