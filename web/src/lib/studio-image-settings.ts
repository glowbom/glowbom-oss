import { withServerAuthHeaders } from './server-auth';
import { studioRequestDeadline } from './studio-deadline';
import { studioProviderName } from './studio-providers';

export interface StudioImageOptions { sourceId: string; modelId: string; aspectRatio: string; resolution: string; quality: string }
export interface StudioImageModel {
  sourceId: string; id: string; name: string; aspectRatios: readonly string[]; defaultAspectRatio: string;
  resolutions: readonly string[]; defaultResolution: string; qualities: readonly string[]; defaultQuality: string;
  pricesUSD?: Readonly<Record<string, number>>; pricesByQualityUSD?: Readonly<Record<string, Readonly<Record<string, number>>>>;
  inputImageUSD?: number; imageOutputUSDPerMillionTokens?: number; textInputUSDPerMillionTokens?: number; imageInputUSDPerMillionTokens?: number;
  pricingURL?: string; pricingAsOf?: string; pricingNotice?: string;
}
export interface StudioImageSource { id: string; name: string; connected: boolean; experimental?: boolean; notice?: string; models: StudioImageModel[] }
export const studioImagePriceCheckedOn = '2026-10-01';
const basic = { aspectRatios: ['1:1', '16:9', '9:16', '4:3', '3:4', '3:2', '2:3'], defaultAspectRatio: '1:1', qualities: [], defaultQuality: '', pricingAsOf: studioImagePriceCheckedOn };
const googlePrice = 'https://ai.google.dev/gemini-api/docs/pricing';
const googleNotice = 'Approximate image output charge. Prompt, reference image, and thinking tokens can add to the total.';
const openAIPrice = 'https://developers.openai.com/api/docs/pricing';
const openAIModern = { ...basic, resolutions: ['1024x1024', '1536x1024', '1024x1536'], defaultResolution: '1024x1024', qualities: ['low', 'medium', 'high', 'xhigh', 'max'], defaultQuality: 'low', pricingURL: openAIPrice, pricingNotice: 'Approximate image output charge. Prompt and reference image tokens are billed separately.' };
const prices25 = Object.fromEntries(['low', 'medium', 'high', 'xhigh', 'max'].map((quality, index) => [quality, { '1024x1024': [0.00588, 0.01317, 0.05268, 0.09366, 0.21072][index]!, '1536x1024': [0.00474, 0.01029, 0.04116, 0.07377, 0.16464][index]!, '1024x1536': [0.00474, 0.01029, 0.04116, 0.07377, 0.16464][index]! }]));
const openAILegacy = { ...openAIModern, qualities: ['low', 'medium', 'high'] };
function openAIPriceTable(square: readonly number[], rectangle: readonly number[]) { return Object.fromEntries(['low', 'medium', 'high'].map((quality, index) => [quality, { '1024x1024': square[index]!, '1536x1024': rectangle[index]!, '1024x1536': rectangle[index]! }])); }
const legacyOpenAIPrices = {
  'gpt-image-2': openAIPriceTable([0.006, 0.053, 0.211], [0.005, 0.041, 0.165]),
  'gpt-image-1.5': openAIPriceTable([0.009, 0.034, 0.133], [0.013, 0.05, 0.2]),
  'gpt-image-1': openAIPriceTable([0.011, 0.042, 0.167], [0.016, 0.063, 0.25]),
  'gpt-image-1-mini': openAIPriceTable([0.005, 0.011, 0.036], [0.006, 0.015, 0.052]),
};

export const studioImageModels: readonly StudioImageModel[] = [
  { ...basic, sourceId: 'gemini-api', id: 'gemini-3.1-flash-lite-image', name: 'Gemini 3.1 Flash Lite Image', resolutions: ['1K'], defaultResolution: '1K', pricesUSD: { '1K': 0.0336 }, pricingURL: googlePrice, pricingNotice: googleNotice },
  { ...basic, sourceId: 'gemini-api', id: 'gemini-3.1-flash-image', name: 'Gemini 3.1 Flash Image', resolutions: ['512', '1K', '2K', '4K'], defaultResolution: '1K', pricesUSD: { '512': 0.045, '1K': 0.067, '2K': 0.101, '4K': 0.151 }, pricingURL: googlePrice, pricingNotice: googleNotice },
  { ...basic, sourceId: 'gemini-api', id: 'gemini-3-pro-image', name: 'Gemini 3 Pro Image', resolutions: ['1K', '2K', '4K'], defaultResolution: '1K', pricesUSD: { '1K': 0.134, '2K': 0.134, '4K': 0.24 }, pricingURL: googlePrice, pricingNotice: googleNotice },
  { ...basic, sourceId: 'xai-api', id: 'grok-imagine-image-2.0', name: 'Grok Imagine Image 2.0', resolutions: ['1k', '2k'], defaultResolution: '1k', qualities: ['low', 'medium'], defaultQuality: 'low', pricesByQualityUSD: { low: { '1k': 0.04, '2k': 0.06 }, medium: { '1k': 0.06, '2k': 0.08 } }, inputImageUSD: 0.01, pricingURL: 'https://docs.x.ai/developers/models/grok-imagine-image-2.0', pricingNotice: 'Approximate provider charge, including the reference image when selected.' },
  { ...basic, sourceId: 'xai-api', id: 'grok-imagine-image', name: 'Grok Imagine Image', resolutions: ['1k', '2k'], defaultResolution: '1k', pricesUSD: { '1k': 0.02, '2k': 0.02 }, inputImageUSD: 0.002, pricingURL: 'https://docs.x.ai/developers/models/grok-imagine-image', pricingNotice: 'Approximate provider charge, including the reference image when selected.' },
  { ...openAIModern, sourceId: 'openai-api', id: 'gpt-image-2.5-flare', name: 'GPT Image 2.5 Flare', pricesByQualityUSD: prices25 },
  { ...openAIModern, sourceId: 'openai-api', id: 'gpt-image-2.5-sunburst', name: 'GPT Image 2.5 Sunburst', pricesByQualityUSD: prices25 },
  { ...openAIModern, sourceId: 'openai-api', id: 'gpt-image-2', name: 'GPT Image 2', qualities: ['low', 'medium', 'high'], pricesByQualityUSD: legacyOpenAIPrices['gpt-image-2'] },
  ...(['gpt-image-1.5', 'gpt-image-1-mini', 'gpt-image-1'] as const).map((id): StudioImageModel => ({ ...openAILegacy, sourceId: 'openai-api', id, name: id === 'gpt-image-1-mini' ? 'GPT Image 1 Mini' : id === 'gpt-image-1.5' ? 'GPT Image 1.5' : 'GPT Image 1', pricesByQualityUSD: legacyOpenAIPrices[id] })),
  { ...basic, sourceId: 'openai-subscription', id: 'gpt-image-2', name: 'GPT Image 2', aspectRatios: ['1:1', '16:9', '9:16'], resolutions: [], defaultResolution: '' },
  { ...basic, sourceId: 'glowbom-api', id: 'flux', name: 'FLUX', resolutions: [], defaultResolution: '', aspectRatios: [], defaultAspectRatio: '' },
];
export const studioImageSources: readonly StudioImageSource[] = [
  ...['gemini-api', 'xai-api', 'openai-api', 'openai-subscription', 'glowbom-api'].map((id) => ({ id, name: studioProviderName(id), connected: false,
    ...(id === 'openai-subscription' ? { notice: 'Your ChatGPT connection supports one image model. Use an OpenAI API key for other models.' } : id === 'glowbom-api' ? { notice: 'Your Glowbom account generates images with FLUX. Other models require a provider API key.' } : {}),
    models: studioImageModels.filter((model) => model.sourceId === id),
  })),
  { id: 'xai-subscription', name: studioProviderName('xai-subscription'), connected: false, notice: 'Media access and limits depend on your Grok account; allowance or purchased credits may apply.', models: studioImageModels.filter((model) => model.sourceId === 'xai-api').map((model) => ({ ...model, sourceId: 'xai-subscription', pricesUSD: undefined, pricesByQualityUSD: undefined })) },
];

export function studioImageModel(sourceId: string, modelId = '', sources: readonly StudioImageSource[] = studioImageSources): StudioImageModel | undefined {
  const models = sources.find((source) => source.id === sourceId)?.models || [];
  return models.find((model) => model.id === modelId) || models[0];
}

export function normalizeStudioImageOptions(value: Partial<StudioImageOptions>, sources: readonly StudioImageSource[] = studioImageSources): StudioImageOptions {
  const source = sources.find((candidate) => candidate.id === value.sourceId) || sources[0];
  const model = source ? studioImageModel(source.id, value.modelId, sources) : undefined;
  if (!source || !model) return { sourceId: value.sourceId || '', modelId: value.modelId || '', aspectRatio: value.aspectRatio || '', resolution: value.resolution || '', quality: value.quality || '' };
  const aspectRatio = model.aspectRatios.includes(value.aspectRatio || '') ? value.aspectRatio! : model.defaultAspectRatio;
  let resolution = model.resolutions.includes(value.resolution || '') ? value.resolution! : model.defaultResolution;
  return { sourceId: source.id, modelId: model.id, aspectRatio, resolution, quality: model.qualities.includes(value.quality || '') ? value.quality! : model.defaultQuality };
}

export function studioImageOptionError(options: StudioImageOptions, sources: readonly StudioImageSource[] = studioImageSources): string {
  const model = sources.find((source) => source.id === options.sourceId)?.models.find((candidate) => candidate.id === options.modelId);
  if (!model) return 'Choose a supported image model.';
  if (model.aspectRatios.length && !model.aspectRatios.includes(options.aspectRatio)) return 'Choose a shape supported by this model.';
  if (model.resolutions.length && !model.resolutions.includes(options.resolution)) return 'Choose a size supported by this model.';
  if (model.qualities.length && !model.qualities.includes(options.quality)) return 'Choose a quality supported by this model.';
  return '';
}

export function studioImageEstimate(options: StudioImageOptions, hasReference = false, sources: readonly StudioImageSource[] = studioImageSources): { usd?: number; label: string; notice: string; pricingURL?: string; checkedOn: string } {
  const model = studioImageModel(options.sourceId, options.modelId, sources);
  const base = { pricingURL: model?.pricingURL, checkedOn: model?.pricingAsOf || studioImagePriceCheckedOn, notice: model?.pricingNotice || 'Your provider sets the final charge.' };
  if (studioImageOptionError(options, sources)) return { ...base, label: 'Choose valid image settings to estimate cost' };
  if (options.sourceId === 'openai-subscription') return { ...base, label: 'Uses your ChatGPT plan allowance', notice: 'Plan usage limits apply. Image generation through the connected Codex account is included in your plan usage.' };
  if (options.sourceId === 'xai-subscription') return { ...base, label: 'Account allowance or credits; check your Grok usage', notice: 'Media access and account limits apply. Your account may use purchased credits.' };
  if (options.sourceId === 'glowbom-api') return { ...base, label: 'Uses your Glowbom account balance', notice: 'Your account shows the applicable balance and limits.' };
  const output = model?.pricesByQualityUSD?.[options.quality]?.[options.resolution] ?? model?.pricesUSD?.[options.resolution];
  if (output === undefined) return { ...base, label: 'Token-based charge; check provider prices' };
  const usd = Number((output + (hasReference ? model?.inputImageUSD || 0 : 0)).toFixed(6));
  const amount = usd.toFixed(usd < 0.02 ? 3 : usd % 0.01 === 0 ? 2 : 4).replace(/0+$/, '').replace(/\.$/, '');
  return { ...base, usd, label: `About $${amount} ${options.sourceId === 'xai-api' ? 'per image' : 'for image output'}` };
}

export async function loadStudioImageCapabilities(signal?: AbortSignal): Promise<StudioImageSource[]> {
  return studioRequestDeadline(async (boundedSignal) => {
    const response = await fetch('/api/studio/images/capabilities', { signal: boundedSignal, cache: 'no-store', headers: withServerAuthHeaders() });
    if (!response.ok) throw new Error((await response.text()).trim().slice(0, 2000) || 'Could not check image models.');
    const result = await response.json() as { sources?: StudioImageSource[] };
    if (!Array.isArray(result.sources)) throw new Error('Restart Glowbom to enable image model selection.');
    return result.sources.map((source) => ({ ...source, name: studioProviderName(source.id), models: source.models.map((model) => ({ ...model, sourceId: source.id })) }));
  }, 30000, 'Checking image models took too long. Refresh and try again.', signal);
}
