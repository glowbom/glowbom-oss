import { notifyImageSettingsChanged, platformOpenAIImageKey, readIconApiKeys } from './project-icon';
import type { ImageProviderKeyState } from '../types/opencode';

export const prototypeImageSources = ['picsum', 'glowbom-api', 'xai-subscription', 'openai-subscription', 'openai-api', 'gemini-api', 'xai-api'] as const;
export type PrototypeImageSource = typeof prototypeImageSources[number];
export type PrototypeImageChoice = { sourceId: PrototypeImageSource; apiKey?: string };
export type PrototypeImages = PrototypeImageChoice & { personalization?: boolean; referencePath?: string };
const storageKey = 'glowbom_prototype_image_source';

export function canonicalImageSource(value: string): Exclude<PrototypeImageSource, 'picsum'> | undefined {
  if (prototypeImageSources.includes(value as PrototypeImageSource) && value !== 'picsum') return value as Exclude<PrototypeImageSource, 'picsum'>;
  if (/^(Glowbom \(Flux\)|Flux)$/i.test(value)) return 'glowbom-api';
  if (!/^(Glowbom Images|Glowby Images|Grok |Nano Banana)/i.test(value)) return undefined;
  if (/chatgpt/i.test(value)) return 'openai-subscription';
  if (/nano banana/i.test(value)) return 'gemini-api';
  if (/grok/i.test(value)) return 'xai-api';
  return 'openai-api';
}

export function readPrototypeImageSource(storage?: Pick<Storage, 'getItem'>): PrototypeImageSource {
  try {
    const settings = storage ?? localStorage;
    const value = settings.getItem(storageKey);
    if (prototypeImageSources.includes(value as PrototypeImageSource)) return value as PrototypeImageSource;
    if (!value) {
      const legacy = settings.getItem('glowbom_oss_image_source') ?? settings.getItem('glowby_oss_image_source');
      if (legacy) return canonicalImageSource(legacy) || 'picsum';
    }
  } catch { /* Use free placeholder photos when storage is unavailable. */ }
  return 'picsum';
}

export function rememberPrototypeImageSource(source: PrototypeImageSource, storage?: Pick<Storage, 'setItem'>) {
  try { (storage ?? localStorage).setItem(storageKey, source); } catch { /* Keep the choice for this session. */ }
  if (!storage) notifyImageSettingsChanged();
}

export function buildImageSettings(sourceId: PrototypeImageSource, enteredKey = '', storedKeys = readIconApiKeys()): { imageSource?: string; imageProviderKeys: ImageProviderKeyState } {
  const keys = { ...storedKeys };
  if (['openai-api', 'gemini-api', 'xai-api'].includes(sourceId) && enteredKey.trim()) keys[sourceId] = enteredKey.trim();
  return {
    ...(sourceId !== 'picsum' ? { imageSource: sourceId } : {}),
    imageProviderKeys: {
      openaiImageKey: platformOpenAIImageKey(keys['openai-api']),
      geminiImageKey: keys['gemini-api'] || '',
      xaiImageKey: keys['xai-api'] || '',
    },
  };
}

export function applyPrototypeImageChoice(current: Required<PrototypeImageChoice>, choice: PrototypeImageChoice): Required<PrototypeImageChoice> {
  const usesKey = ['openai-api', 'gemini-api', 'xai-api'].includes(choice.sourceId);
  const apiKey = choice.apiKey?.trim() || (choice.sourceId === current.sourceId ? current.apiKey : '');
  return { sourceId: choice.sourceId, apiKey: usesKey ? apiKey : '' };
}

export function prototypeImageRequest(sourceId: PrototypeImageSource, enteredKey: string, storedKeys = readIconApiKeys()): PrototypeImages {
  const apiKey = enteredKey.trim() || storedKeys[sourceId] || '';
  return { sourceId, ...(['openai-api', 'gemini-api', 'xai-api'].includes(sourceId) && apiKey ? { apiKey } : {}) };
}

export function withPrototypeReference(images: PrototypeImages, enabled: boolean, referencePath?: string): PrototypeImages {
  const { personalization: _personalization, referencePath: _referencePath, ...source } = images;
  if (!enabled) return source;
  if (source.sourceId === 'picsum') throw new Error('Choose Glowbom Images in settings to personalize your images.');
  if (!referencePath?.trim()) throw new Error('Add a photo or drawing to personalize your images.');
  return { ...source, personalization: true, referencePath };
}
