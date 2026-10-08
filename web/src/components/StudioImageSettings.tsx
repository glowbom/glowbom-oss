import { normalizeStudioImageOptions, studioImageEstimate, studioImageSources, type StudioImageOptions, type StudioImageSource } from '../lib/studio-image-settings';
import { StudioProviderKey } from './StudioProviderKey';
import type { useStudioProviderKey } from '../hooks/useStudioProviderKey';
import { studioProviderKeySource } from '../lib/studio-providers';
import './studio-image-settings.css';

export interface StudioImageSettingsProps {
  options: StudioImageOptions; onChange(options: StudioImageOptions): void; sources?: readonly StudioImageSource[];
  keySettings?: ReturnType<typeof useStudioProviderKey>; disabled?: boolean; hasReference?: boolean; hideProvider?: boolean; hideKey?: boolean;
  loading?: boolean; error?: string; onRefresh?(): void; onAccount?(): void;
}

export function StudioImageSettings({ options, onChange, sources = studioImageSources, keySettings, disabled = false, hasReference = false, hideProvider = false, hideKey = false, loading = false, error = '', onRefresh, onAccount }: StudioImageSettingsProps) {
  const source = sources.find((candidate) => candidate.id === options.sourceId), model = source?.models.find((candidate) => candidate.id === options.modelId);
  const estimate = studioImageEstimate(options, hasReference, sources);
  const chatGPTSubscription = options.sourceId === 'openai-subscription';
  const connectionStatus = loading ? 'Checking image providers…'
    : chatGPTSubscription ? source?.connected ? 'ChatGPT sign-in found. Access is checked when you generate.' : 'Sign in to ChatGPT in OpenCode or Codex, then check again.'
    : source?.connected || keySettings?.ready ? 'Provider ready.'
    : studioProviderKeySource(options.sourceId) ? 'Enter an API key or save one to create an image.' : 'Connect this account, then refresh.';
  function update(changes: Partial<StudioImageOptions>) { onChange(normalizeStudioImageOptions({ ...options, ...changes }, sources)); }
  return <div className="studio-image-settings">
    <div className="studio-image-option-grid">
      {!hideProvider && <label>Provider<select value={options.sourceId} disabled={disabled || loading} onChange={(event) => update({ sourceId: event.target.value, modelId: '', resolution: '', quality: '' })}>
        {!options.sourceId && <option value="">{loading ? 'Checking providers…' : 'Choose an image provider'}</option>}
        {!!options.sourceId && !source && <option value={options.sourceId}>{options.sourceId} (unavailable)</option>}
        {sources.map((candidate) => <option key={candidate.id} value={candidate.id}>{candidate.name}</option>)}
      </select></label>}
      <label>Model<select value={options.modelId} disabled={disabled || loading || !source} onChange={(event) => update({ modelId: event.target.value })}>
        {!options.modelId && <option value="">Choose an image model</option>}
        {!!options.modelId && !source?.models.some((candidate) => candidate.id === options.modelId) && <option value={options.modelId}>{options.modelId} (unavailable)</option>}
        {source?.models.map((candidate) => <option key={candidate.id} value={candidate.id}>{candidate.name}</option>)}
      </select></label>
    </div>
    {!hideKey && keySettings && studioProviderKeySource(options.sourceId) && <StudioProviderKey sourceId={options.sourceId} settings={keySettings} connected={source?.connected} disabled={disabled} />}
    {source?.notice && <p className="studio-generator-note">{source.notice}</p>}
    <div className="studio-image-option-grid">
      {!!model?.resolutions.length && <label>Size<select value={options.resolution} disabled={disabled} onChange={(event) => update({ resolution: event.target.value })}>{model.resolutions.map((size) => <option key={size} value={size}>{size.includes('x') ? `${size.replace('x', ' × ')} pixels` : size.toUpperCase()}</option>)}</select></label>}
      {!!model?.qualities.length && <label>Quality<select value={options.quality} disabled={disabled} onChange={(event) => update({ quality: event.target.value })}>{model.qualities.map((quality) => <option key={quality} value={quality}>{quality === 'xhigh' ? 'Extra high' : quality === 'max' ? 'Maximum' : quality[0]!.toUpperCase() + quality.slice(1)}</option>)}</select></label>}
      {!!model?.aspectRatios.length && <label>Shape<select value={options.aspectRatio} disabled={disabled} onChange={(event) => update({ aspectRatio: event.target.value })}>{model.aspectRatios.map((ratio) => <option key={ratio} value={ratio}>{ratio === '1:1' ? 'Square' : ratio === '16:9' ? 'Landscape' : ratio === '9:16' ? 'Portrait' : ratio}</option>)}</select></label>}
    </div>
    {options.sourceId === 'openai-api' && <p className="studio-generator-note">The selected size is billed before Glowbom crops to your shape. Prompt and reference image tokens cost extra.</p>}
    <div className="studio-image-cost" aria-live="polite"><strong>{estimate.label}</strong><p>{estimate.notice}</p>{estimate.pricingURL && studioProviderKeySource(options.sourceId) && <small><a href={estimate.pricingURL} target="_blank" rel="noreferrer">Provider prices</a> checked {estimate.checkedOn}</small>}</div>
    {error && <p className="studio-image-error" role="alert">{error}</p>}
    {onRefresh && <div className="studio-image-connection"><p className="studio-generator-note" role="status">{connectionStatus}</p><button type="button" disabled={disabled || loading} onClick={onRefresh}>{chatGPTSubscription ? 'Check sign-in' : 'Refresh connection'}</button>{options.sourceId === 'glowbom-api' && onAccount && <button type="button" disabled={disabled} onClick={onAccount}>Open Account</button>}</div>}
  </div>;
}
