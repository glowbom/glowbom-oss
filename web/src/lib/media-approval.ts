import type { OpenCodeMediaApproval, OpenCodeMediaApprovalItem, ProjectIconSource } from '../types/opencode';
import { imageSourceHelp } from './project-icon';
import { normalizeStudioVideoOptions, studioVideoCredentials, studioVideoOptionError, type StudioVideoOptions, type StudioVideoSource } from './studio-video';
import { normalizeStudioImageOptions, studioImageOptionError, studioImageSources, type StudioImageOptions } from './studio-image-settings';
import { studioProviderCredentials } from './studio-providers';

const textValue = (value: unknown): string => typeof value === 'string' ? value : '';
const numberValue = (value: unknown): number | undefined => typeof value === 'number' && Number.isFinite(value) ? value : undefined;
export const mediaApprovalMusicDuration = 30;

export function normalizeMediaApproval(raw: unknown): OpenCodeMediaApproval | null {
  if (!raw || typeof raw !== 'object') return null;
  const record = raw as Record<string, unknown>;
  const id = textValue(record.id);
  if (!id) return null;
  const items = (Array.isArray(record.items) ? record.items : []).flatMap((value, index): OpenCodeMediaApprovalItem[] => {
    if (!value || typeof value !== 'object') return [];
    const item = value as Record<string, unknown>;
    const prompt = textValue(item.prompt);
    const mediaType = textValue(item.mediaType);
    if (!prompt || !mediaType) return [];
    const referenceImages = Array.isArray(item.referenceImages) ? item.referenceImages.filter((image): image is string => typeof image === 'string') : [];
    return [{
      id: textValue(item.id) || `asset-${index + 1}`,
      mediaType, prompt, provider: textValue(item.provider),
      placeholder: textValue(item.placeholder) || undefined,
      sourceId: textValue(item.sourceId) || undefined,
      excluded: item.excluded === true,
      referenceImages,
      referenceOrigin: mediaType === 'image' && referenceImages.some((reference) => reference.trim()) && item.referenceOrigin === 'previous-image' ? 'previous-image' : undefined,
      aspectRatio: typeof item.aspectRatio === 'string' ? item.aspectRatio : undefined,
      fromKey: textValue(item.fromKey) || undefined,
      audioType: textValue(item.audioType) || undefined,
      voiceID: textValue(item.voiceID) || undefined,
      modelID: textValue(item.modelID) || undefined,
      durationSeconds: numberValue(item.durationSeconds),
      resolution: textValue(item.resolution) || undefined,
      quality: textValue(item.quality) || undefined,
      promptInfluence: numberValue(item.promptInfluence),
      loop: item.loop === true,
      forceInstrumental: item.forceInstrumental === true,
      usagePrompt: textValue(item.usagePrompt) || undefined,
    }];
  });
  return items.length ? { id, title: textValue(record.title) || 'Create these assets?', message: textValue(record.message), items: mediaApprovalDraft(items) } : null;
}

export function mediaApprovalDraft(items: readonly OpenCodeMediaApprovalItem[]): OpenCodeMediaApprovalItem[] {
  const images = items.filter((item) => item.mediaType === 'image');
  return items.map((item) => {
    const startingImage = item.mediaType === 'video' && item.fromKey
      ? images.find((image) => [image.id, image.prompt, image.placeholder].includes(item.fromKey))
      : undefined;
    const defaultMusicDuration = item.mediaType === 'audio' && item.audioType === 'music' && (item.durationSeconds === undefined || item.durationSeconds === 0);
    const referenceImages = [...(item.referenceImages || [])];
    const referenceOrigin = item.mediaType === 'image' && referenceImages.some((reference) => typeof reference === 'string' && reference.trim()) && item.referenceOrigin === 'previous-image' ? 'previous-image' : undefined;
    const videoDefaults = item.mediaType === 'video' ? normalizeStudioVideoOptions({ sourceId: item.sourceId as StudioVideoOptions['sourceId'], modelId: item.modelID, resolution: item.resolution, durationSeconds: item.durationSeconds, aspectRatio: item.aspectRatio }) : undefined;
    const imageDefaults = item.mediaType === 'image' && studioImageSources.some((source) => source.id === item.sourceId) ? mediaApprovalImageOptions(item) : undefined;
    return { ...item, ...(defaultMusicDuration ? { durationSeconds: mediaApprovalMusicDuration } : {}), ...(imageDefaults ? { modelID: imageDefaults.modelId, resolution: imageDefaults.resolution, quality: imageDefaults.quality, aspectRatio: imageDefaults.aspectRatio } : {}), ...(videoDefaults ? { modelID: item.modelID || videoDefaults.modelId, durationSeconds: item.durationSeconds || videoDefaults.durationSeconds, resolution: item.resolution || videoDefaults.resolution, aspectRatio: item.aspectRatio || videoDefaults.aspectRatio } : {}), ...(startingImage ? { fromKey: startingImage.id } : {}), referenceImages, referenceOrigin };
  });
}

export function mediaApprovalReferencePatch(image?: string): Pick<OpenCodeMediaApprovalItem, 'referenceImages' | 'referenceOrigin'> {
  return { referenceImages: image ? [image] : [], referenceOrigin: undefined };
}

export function mediaApprovalImageConnections(items: readonly OpenCodeMediaApprovalItem[], sources: readonly ProjectIconSource[]): ProjectIconSource[] {
  const apiSources = ['openai-api', 'gemini-api', 'xai-api'];
  const selectedIDs = new Set(items.filter((item) => !item.excluded).flatMap((item): string[] =>
    item.mediaType === 'video' && item.sourceId === 'veo-api' ? ['gemini-api']
      : item.mediaType === 'video' && item.sourceId === 'xai-api' ? ['xai-api']
      : item.mediaType === 'image' && item.sourceId && apiSources.includes(item.sourceId) ? [item.sourceId] : []));
  const providerNames: Record<string, string> = { 'openai-api': 'OpenAI', 'gemini-api': 'Google', 'xai-api': 'SpaceXAI' };
  return [...selectedIDs].flatMap((id): ProjectIconSource[] => {
    const source = sources.find((candidate) => candidate.id === id);
    if (source && source.authType !== 'api-key') return [];
    return [source || { id, label: providerNames[id] || id, model: '', authType: 'api-key', available: false }];
  });
}

export function newMediaApprovalItem(mediaType: 'image' | 'video' | 'audio', id = `added-${crypto.randomUUID()}`): OpenCodeMediaApprovalItem {
  return {
    id, mediaType, prompt: '', provider: '', usagePrompt: '', referenceImages: [],
    sourceId: mediaType === 'video' ? 'veo-api' : mediaType === 'audio' ? 'elevenlabs-api' : '',
    aspectRatio: mediaType === 'video' ? '16:9' : undefined,
    ...(mediaType === 'video' ? { modelID: 'veo-3.1-lite-generate-preview', durationSeconds: 4, resolution: '720p' } : {}),
    ...(mediaType === 'audio' ? { audioType: 'sound' } : {}),
  };
}

export function mediaApprovalItemError(item: OpenCodeMediaApprovalItem): string {
  if (item.excluded) return '';
  if (!item.prompt.trim()) return 'Describe the asset to create.';
  if (item.prompt.length > 10000) return 'Keep the asset prompt under 10,000 characters.';
  if (item.id.startsWith('added-') && !item.usagePrompt?.trim()) return 'Describe how the app should use this added asset.';
  if ((item.usagePrompt?.length || 0) > 10000) return 'Keep the usage instruction under 10,000 characters.';
  if (!item.sourceId) return 'Choose a source for this asset.';
  if (item.mediaType === 'image') {
    if ((item.referenceImages?.length || 0) > 1) return 'Use one reference image for this asset.';
    if (item.referenceImages?.some((image) => !/^data:image\/(?:png|jpeg|webp);base64,[A-Za-z0-9+/]+=*$/.test(image))) return 'Upload a PNG, JPEG, or WebP reference photo.';
    if (item.sourceId === 'glowbom-api' && item.aspectRatio) return 'Use the default image size for your Glowbom account.';
    const optionError = studioImageOptionError(mediaApprovalImageOptions(item));
    if (optionError) return optionError;
  } else if (item.mediaType === 'video') {
    const optionError = studioVideoOptionError(mediaApprovalVideoOptions(item));
    if (optionError) return optionError;
    if (!item.fromKey?.trim()) return 'Choose an image prompt or saved image ID for the starting frame.';
  } else if (item.mediaType === 'audio') {
    if (item.sourceId !== 'elevenlabs-api') return 'Choose ElevenLabs for this audio.';
    if (!['voice', 'sound', 'music'].includes(item.audioType || '')) return 'Choose voice, sound, or music.';
    if (item.modelID !== undefined && !item.modelID.trim()) return 'Choose a model or enter a custom model ID.';
    if ((item.voiceID?.length || 0) > 200 || (item.modelID?.length || 0) > 200) return 'Keep the voice and model IDs under 200 characters.';
    if (item.modelID && !/^[A-Za-z0-9._:-]+$/.test(item.modelID)) return 'Use a model ID containing letters, numbers, dots, underscores, colons, or hyphens.';
    if (item.audioType === 'voice' && item.modelID === 'eleven_v4_turbo') return 'Eleven v4 Turbo requires a streaming voice connection. Choose another model.';
    if (item.audioType === 'voice' && item.modelID === 'eleven_v4' && item.prompt.length > 2000) return 'Keep Eleven v4 passages under 2,000 characters.';
    if (item.audioType === 'voice' && (item.durationSeconds || item.promptInfluence !== undefined || item.loop || item.forceInstrumental)) return 'Duration, prompt influence, looping, and instrumental options do not apply to voice. Choose the audio type again to reset them.';
    if (item.audioType === 'sound' && (item.voiceID || item.forceInstrumental)) return 'Voice and instrumental options do not apply to sound. Choose the audio type again to reset them.';
    if (item.audioType === 'sound' && item.loop && item.modelID && item.modelID !== 'eleven_text_to_sound_v2') return 'Looping requires the ElevenLabs sound effects v2 model.';
    if (item.audioType === 'music' && (item.voiceID || item.promptInfluence !== undefined || item.loop)) return 'Voice, prompt influence, and looping do not apply to music. Choose the audio type again to reset them.';
    const duration = item.durationSeconds;
    if (item.audioType === 'music' && (duration === undefined || !Number.isFinite(duration) || duration < 3 || duration > 600)) return 'Choose a music duration from 3 to 600 seconds.';
    if (item.audioType === 'sound' && duration !== undefined && (!Number.isFinite(duration) || (duration !== 0 && (duration < 0.5 || duration > 30)))) return 'Sound duration must be 0.5 to 30 seconds, or empty for automatic.';
    if (item.promptInfluence !== undefined && (!Number.isFinite(item.promptInfluence) || item.promptInfluence < 0 || item.promptInfluence > 1)) return 'Prompt influence must be between 0 and 1.';
  } else return 'This asset type is not supported. Exclude it to continue.';
  return '';
}

export function mediaApprovalSourceError(item: OpenCodeMediaApprovalItem, sources: readonly ProjectIconSource[], keys: Record<string, string>, sourcesLoaded: boolean, videoSources?: readonly StudioVideoSource[]): string {
  if (!item.excluded && item.mediaType === 'video' && videoSources) {
    const source = videoSources.find((candidate) => candidate.id === item.sourceId);
    if (!source) return 'This video source is unavailable. Refresh sources or choose another source.';
    const credentials = studioVideoCredentials(source.id);
    if (source.connected || credentials.apiKey || credentials.useSavedKey) return '';
    return source.requiresApiKey ? 'Add this provider’s API key under Media connections.' : 'Connect Grok through OpenCode, then refresh sources.';
  }
  if (item.excluded || item.mediaType !== 'image' || !item.sourceId || !sourcesLoaded) return '';
  const source = sources.find((candidate) => candidate.id === item.sourceId);
  if (!source) return 'This image source is unavailable. Refresh sources or choose another source.';
  const credentials = studioProviderCredentials(source.id);
  if (source.available || (source.authType === 'api-key' && (keys[source.id] || credentials.apiKey || credentials.useSavedKey))) return '';
  return source.authType === 'api-key' ? 'Add this provider’s API key under Media connections, or connect it in Settings.' : imageSourceHelp(source);
}

export function mediaApprovalImageOptions(item: OpenCodeMediaApprovalItem): StudioImageOptions {
  const defaults = normalizeStudioImageOptions({ sourceId: item.sourceId, modelId: item.modelID, aspectRatio: item.aspectRatio, resolution: item.resolution, quality: item.quality });
  return { sourceId: item.sourceId || '', modelId: item.modelID || defaults.modelId, aspectRatio: item.aspectRatio || defaults.aspectRatio, resolution: item.resolution || defaults.resolution, quality: item.quality || defaults.quality };
}

export function updateMediaImageOptions(options: StudioImageOptions): Partial<OpenCodeMediaApprovalItem> {
  return { sourceId: options.sourceId, modelID: options.modelId, aspectRatio: options.aspectRatio, resolution: options.resolution, quality: options.quality };
}

export function mediaApprovalVideoOptions(item: OpenCodeMediaApprovalItem): StudioVideoOptions {
  return { sourceId: item.sourceId as StudioVideoOptions['sourceId'], modelId: item.modelID || '', resolution: item.resolution || '', durationSeconds: item.durationSeconds ?? 0, aspectRatio: item.aspectRatio || '16:9' };
}

export function updateMediaVideoOptions(item: OpenCodeMediaApprovalItem, patch: Partial<StudioVideoOptions>): Partial<OpenCodeMediaApprovalItem> {
  const options = normalizeStudioVideoOptions({ ...mediaApprovalVideoOptions(item), ...patch });
  return { sourceId: options.sourceId, modelID: options.modelId, durationSeconds: options.durationSeconds, resolution: options.resolution, aspectRatio: options.aspectRatio };
}

export function mediaApprovalGenerateError(items: readonly OpenCodeMediaApprovalItem[]): string {
  if (!items.some((item) => !item.excluded)) return 'Include at least one asset to create, or choose Skip all.';
  for (let index = 0; index < items.length; index++) {
    const item = items[index]!;
    const error = mediaApprovalItemError(item);
    if (error) return `Asset ${index + 1}: ${error}`;
  }
  for (const item of items) {
    if (item.excluded || item.mediaType !== 'video' || !item.fromKey) continue;
    const startingImage = items.find((candidate) => candidate.mediaType === 'image' && [candidate.id, candidate.prompt, candidate.placeholder].includes(item.fromKey));
    if (startingImage?.excluded) return 'Include the video’s starting image or choose another starting frame.';
  }
  return '';
}

export function updateMediaAudioType(item: OpenCodeMediaApprovalItem, audioType: 'voice' | 'sound' | 'music'): OpenCodeMediaApprovalItem {
  return { ...item, audioType, modelID: undefined, voiceID: audioType === 'voice' ? item.voiceID : undefined, durationSeconds: audioType === 'music' ? mediaApprovalMusicDuration : undefined, promptInfluence: undefined, loop: false, forceInstrumental: false };
}
