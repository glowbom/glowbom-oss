export const sketchTextFont = 'system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif';

export interface SketchText {
  kind: 'text';
  text: string;
  x: number;
  y: number;
  width: number;
  fontSize: number;
  color: string;
}

type Measure = (text: string, fontSize: number) => number;

export function wrapSketchText(text: string, width: number, fontSize: number, measure: Measure): string[] {
  const lines: string[] = [];
  for (const paragraph of text.split('\n')) {
    let line = '';
    for (const word of paragraph.split(/\s+/)) {
      if (!word) continue;
      const combined = line ? `${line} ${word}` : word;
      if (measure(combined, fontSize) <= width) { line = combined; continue; }
      if (line) { lines.push(line); line = ''; }
      for (const character of Array.from(word)) {
        if (line && measure(line + character, fontSize) > width) { lines.push(line); line = ''; }
        line += character;
      }
    }
    lines.push(line);
  }
  return lines;
}

// All stored geometry is relative to the image, including the text size.
export function placeSketchText(x: number, y: number, width: number, height: number, color: string): SketchText {
  const font = Math.min(20, width / 8, height / 3);
  const margin = font * 0.35;
  const minimumWidth = Math.min(160, width - margin * 2);
  const left = Math.max(margin, Math.min(x * width, width - minimumWidth - margin));
  const top = Math.max(margin, Math.min(y * height, height - font * 2.6 - margin));
  return { kind: 'text', text: '', x: left / width, y: top / height, width: Math.min(320, width - left - margin) / width, fontSize: font / width, color };
}

export function layoutSketchText(text: SketchText, width: number, height: number, measure: Measure) {
  let fontSize = text.fontSize * width;
  const margin = fontSize * 0.35;
  const boxWidth = Math.max(fontSize, Math.min(text.width * width, width - margin * 2));
  let lines = wrapSketchText(text.text, boxWidth, fontSize, measure);
  // Long comments still fit within the image instead of being clipped on export.
  for (let attempt = 0; attempt < 8 && lines.length * fontSize * 1.3 > height - margin * 2; attempt++) {
    fontSize *= Math.min(0.9, (height - margin * 2) / (lines.length * fontSize * 1.3));
    lines = wrapSketchText(text.text, boxWidth, fontSize, measure);
  }
  const lineHeight = fontSize * 1.3;
  const boxHeight = lines.length * lineHeight;
  return {
    lines, fontSize, lineHeight, width: boxWidth, height: boxHeight,
    x: Math.max(margin, Math.min(text.x * width, width - boxWidth - margin)),
    y: Math.max(margin, Math.min(text.y * height, height - boxHeight - margin)),
  };
}
