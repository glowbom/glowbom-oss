import { useEffect, useRef, useState } from 'react';
import { openCodeApi, toErrorMessage } from '../lib/api';
import { imageSettingsChangedEvent, imageSourceHelp, imageSourceName, imageSourceOptionLabel, preferredIconSource, readIconApiKeys, rememberImageSessionKey } from '../lib/project-icon';
import type { PrototypeImageSource } from '../lib/prototype-images';
import type { ProjectIconSource } from '../types/opencode';
import { accountStatusChangedEvent } from '../lib/account';
import type { OnboardingPreference } from '../lib/onboarding';
import { recommendOnboardingImages } from '../lib/onboarding-image-recommendations';
import './prototype-image-settings.css';

export function PrototypeImageSettings({ sourceId, apiKey, disabled, onSourceChange, onKeyChange, context = 'prototype', showKeyInput = true, onOpenAccount, onSourcesChange, recommendationPreference, automaticRecommendation = false, onUseRecommendation }: { sourceId: PrototypeImageSource; apiKey: string; disabled: boolean; onSourceChange(value: PrototypeImageSource): void; onKeyChange(value: string): void; context?: 'prototype' | 'onboarding' | 'assets'; showKeyInput?: boolean; onOpenAccount?(): void; onSourcesChange?(sources: ProjectIconSource[]): void; recommendationPreference?: OnboardingPreference; automaticRecommendation?: boolean; onUseRecommendation?(sourceId: PrototypeImageSource): void }) {
  const [sources, setSources] = useState<ProjectIconSource[]>([]);
  const [keys, setKeys] = useState(readIconApiKeys);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [attempt, setAttempt] = useState(0);
  const [checkedAttempt, setCheckedAttempt] = useState<number | null>(null);
  const notifySources = useRef(onSourcesChange);
  useEffect(() => { notifySources.current = onSourcesChange; }, [onSourcesChange]);
  const source = sources.find((item) => item.id === sourceId);
  const knownKeys = Object.fromEntries(Object.entries(keys).map(([id, key]) => [id, !!key.trim()]));
  if (apiKey.trim()) knownKeys[sourceId] = true;
  const recommendationsReady = !loading && !error && checkedAttempt === attempt;
  const recommendation = context === 'onboarding' && recommendationPreference && recommendationsReady
    ? recommendOnboardingImages(sources, recommendationPreference, knownKeys) : null;
  const recommendedSource = sources.find(item => item.id === recommendation?.sourceId);
  useEffect(() => {
    const refresh = () => setAttempt((value) => value + 1);
    window.addEventListener(accountStatusChangedEvent, refresh);
    return () => window.removeEventListener(accountStatusChangedEvent, refresh);
  }, []);
  useEffect(() => {
    const refreshKeys = () => setKeys(readIconApiKeys());
    window.addEventListener(imageSettingsChangedEvent, refreshKeys);
    return () => window.removeEventListener(imageSettingsChangedEvent, refreshKeys);
  }, []);
  const generated = sourceId !== 'picsum';
  function chooseSource(nextId: PrototypeImageSource) {
    onSourceChange(nextId);
    if (sources.find((item) => item.id === nextId)?.authType === 'account') {
      setLoading(true);
      setAttempt((value) => value + 1);
    }
  }
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true); setError('');
    void openCodeApi.getIconSources(controller.signal).then((result) => {
      if (!controller.signal.aborted) { setSources(result.sources); setKeys(readIconApiKeys()); setCheckedAttempt(attempt); notifySources.current?.(result.sources); }
    }).catch((err) => {
      if (!controller.signal.aborted) {
        setError(toErrorMessage(err, 'Could not load image sources.'));
        setSources((current) => current.map((item) => item.authType === 'account' ? { ...item, available: false, availabilityCode: 'account_unavailable' } : item));
        notifySources.current?.([]);
      }
    })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [attempt]);
  useEffect(() => {
    if (!disabled && automaticRecommendation && recommendation && recommendation.sourceId !== sourceId) onUseRecommendation?.(recommendation.sourceId);
  }, [disabled, automaticRecommendation, recommendation?.sourceId, sourceId, onUseRecommendation]);
  return <div className="prototype-image-settings">
    {context === 'onboarding' || context === 'assets' ? <label>Image provider and model<select value={sourceId} disabled={disabled || loading} onChange={(event) => chooseSource(event.target.value as PrototypeImageSource)}>
      {context === 'onboarding' && <option value="picsum">Free placeholder photos (Lorem Picsum)</option>}
      {generated && !sources.some(item => item.id === sourceId) && <option value={sourceId}>{loading ? 'Loading your image source…' : 'Saved image source unavailable'}</option>}
      {sources.map(item => <option key={item.id} value={item.id} disabled={item.authType === 'subscription' && !item.available}>{imageSourceOptionLabel(item, keys)} · {item.model}</option>)}
    </select></label> : <label>Prototype images<select value={generated ? 'generated' : 'picsum'} disabled={disabled} onChange={(event) => {
      chooseSource(event.target.value === 'picsum' ? 'picsum' : (preferredIconSource(sources, keys) || 'openai-api') as PrototypeImageSource);
    }}><option value="picsum">Lorem Picsum</option><option value="generated" disabled={loading || !sources.length}>Glowbom Images</option></select></label>}
    {error && <p role="alert">{error} <button type="button" disabled={disabled || loading} onClick={() => setAttempt((value) => value + 1)}>Retry sources</button></p>}
    {context === 'onboarding' && recommendationPreference && <div className="prototype-image-recommendation" aria-live="polite">
      {error ? <p>Check image sources again to see a recommendation.</p> : loading || checkedAttempt !== attempt ? <p role="status">Checking image recommendations…</p> : recommendation && <>
        <small>Recommended for your setup</small>
        <strong>{recommendedSource && recommendation.model ? `${imageSourceName(recommendedSource)} · ${recommendation.model}` : 'Free placeholder photos'}</strong>
        <p>{recommendation.reason}</p>
        {recommendation.sourceId !== sourceId && onUseRecommendation && <button type="button" disabled={disabled} onClick={() => onUseRecommendation(recommendation.sourceId)}>Use recommended images</button>}
      </>}
    </div>}
    {generated ? <>
      {context === 'prototype' && <label>Image source<select value={sourceId} disabled={disabled || loading || !sources.length} onChange={(event) => chooseSource(event.target.value as PrototypeImageSource)}>
        {!sources.length && <option value={sourceId}>{loading ? 'Loading sources…' : 'Sources unavailable'}</option>}
        {sources.map((item) => <option key={item.id} value={item.id} disabled={item.authType === 'subscription' && !item.available}>{imageSourceOptionLabel(item, keys)}</option>)}
      </select></label>}
      {source && (loading && source.authType === 'account' ? <p role="status">Checking your Glowbom account…</p> : <p>{source.model}. {imageSourceHelp(source)}</p>)}
      {source?.authType === 'account' && !source.available && onOpenAccount && <button type="button" disabled={disabled} onClick={onOpenAccount}>Open Glowbom account</button>}
      {(source?.authType === 'account' || source?.authType === 'subscription') && !source.available && !loading && <button type="button" disabled={disabled} onClick={() => setAttempt((value) => value + 1)}>Refresh sources</button>}
      {showKeyInput && source?.authType === 'api-key' && <label>{source.available || keys[sourceId] ? 'Use another API key (optional)' : 'Image API key (optional)'}<input type="password" value={apiKey} autoComplete="off" spellCheck={false} disabled={disabled} onChange={(event) => { rememberImageSessionKey(sourceId, event.target.value); onKeyChange(event.target.value); }} /><small>Shared with app icons and Build for this session.{context === 'onboarding' ? ' You can also add it in Draw when you need it.' : ''}</small></label>}
      <p>{context === 'onboarding' ? 'This image source will be used when you build a prototype in Draw. Setup does not generate images or check your remaining allowance.' : context === 'assets' ? 'Build lets you review each asset, change its source and prompt, add a reference, or exclude it before generation.' : 'Build creates up to four images after the prototype. Generated images are saved in the project and Studio.'}</p>
    </> : <p>Free placeholder photos, saved in your project. {context === 'onboarding' ? 'Choose an image provider above to create images for your idea.' : 'Choose Glowbom Images to create images for your idea.'}</p>}
  </div>;
}
