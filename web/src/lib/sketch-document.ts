import type { SketchText } from './sketch-text';

export type SketchPoint = { x: number; y: number };
export type SketchStroke = { kind: 'stroke'; points: SketchPoint[]; erase: boolean; color: string; width?: number };
export type SketchShape = { kind: 'shape'; shape: 'rectangle' | 'arrow'; start: SketchPoint; end: SketchPoint; color: string; width?: number };
export type SketchAnnotation = SketchStroke | SketchShape | SketchText;

// Geometry stays editable. Only an imported photograph or screenshot is a bitmap.
export interface SketchDocument {
  version: 1;
  width: number;
  height: number;
  annotations: SketchAnnotation[];
  background?: string;
}

export const maxSketchBackgroundCharacters = 4 * 1024 * 1024;

export function isSketchDocument(value: unknown): value is SketchDocument {
  if (!value || typeof value !== 'object') return false;
  const item = value as SketchDocument;
  const finite = (number: unknown): number is number => typeof number === 'number' && Number.isFinite(number);
  const point = (value: unknown): value is SketchPoint => !!value && typeof value === 'object' && finite((value as SketchPoint).x) && finite((value as SketchPoint).y);
  return item.version === 1 && finite(item.width) && item.width > 0 && finite(item.height) && item.height > 0
    && (item.background === undefined || (typeof item.background === 'string' && /^data:image\/(png|jpeg);base64,[A-Za-z0-9+/]+=*$/.test(item.background) && item.background.length <= maxSketchBackgroundCharacters))
    && Array.isArray(item.annotations) && item.annotations.every((annotation) => {
      if (!annotation || typeof annotation !== 'object' || typeof annotation.color !== 'string') return false;
      if (annotation.kind === 'text') return typeof annotation.text === 'string' && finite(annotation.x) && finite(annotation.y) && finite(annotation.width) && annotation.width > 0 && finite(annotation.fontSize) && annotation.fontSize > 0;
      if (annotation.width !== undefined && (!finite(annotation.width) || annotation.width <= 0)) return false;
      if (annotation.kind === 'stroke') return typeof annotation.erase === 'boolean' && Array.isArray(annotation.points) && annotation.points.every(point);
      return annotation.kind === 'shape' && (annotation.shape === 'rectangle' || annotation.shape === 'arrow') && point(annotation.start) && point(annotation.end);
    });
}

export function snapshotSketchDocument(width: number, height: number, annotations: readonly SketchAnnotation[], background?: string): SketchDocument {
  if (!Number.isFinite(width) || !Number.isFinite(height) || width <= 0 || height <= 0) throw new Error('Could not read the sketch dimensions.');
  if (background && (!/^data:image\/(png|jpeg);base64,[A-Za-z0-9+/]+=*$/.test(background) || background.length > maxSketchBackgroundCharacters)) throw new Error('The sketch background is too large to save. Choose a smaller image.');
  const snapshot: SketchDocument = { version: 1, width, height, annotations: structuredClone(annotations) as SketchAnnotation[] };
  if (background) snapshot.background = background;
  return snapshot;
}
