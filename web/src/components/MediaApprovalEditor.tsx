import { useEffect, useId, useRef, useState } from 'react';
import { toErrorMessage } from '../lib/api';
import { accountStatusChangedEvent } from '../lib/account';
import { mediaApprovalDraft, mediaApprovalGenerateError, mediaApprovalImageConnections, mediaApprovalImageOptions, mediaApprovalItemError, mediaApprovalReferencePatch, mediaApprovalSourceError, mediaApprovalVideoOptions, newMediaApprovalItem, updateMediaAudioType, updateMediaImageOptions, updateMediaVideoOptions } from '../lib/media-approval';
import { imageSettingsChangedEvent, imageSourceHelp, imageSourceName, readIconApiKeys, readIconReference, readImageSessionKey, rememberImageSessionKey } from '../lib/project-icon';
import type { OpenCodeMediaApproval, OpenCodeMediaApprovalItem, OpenCodeMediaApprovalRespondRequest, ProjectIconSource } from '../types/opencode';
import { studioVideoDurations, studioVideoEstimate, studioVideoModel, studioVideoSources, type StudioVideoSource, type StudioVideoSourceID } from '../lib/studio-video';
import { studioImageEstimate, studioImageModel, studioImageSources, type StudioImageSource } from '../lib/studio-image-settings';
import { studioProviderCredentials, studioProviderKeyLabel, studioProviderKeySource, studioProviderName } from '../lib/studio-providers';
import { loadMediaApprovalImageSources, loadMediaApprovalVideoSources } from '../lib/media-approval-sources';
import { StudioImageSettings } from './StudioImageSettings';
import { ElevenLabsModelSelector } from './ElevenLabsModelSelector';
import { defaultStudioAudioDraft, type StudioAudioMode } from '../lib/studio-audio';
import { useMediaApprovalDraft, type MediaApprovalDraftController } from '../hooks/useMediaApprovalDraft';
import './media-approval-editor.css';

type MediaKind = 'image' | 'video' | 'audio';
interface Props {
  approval: OpenCodeMediaApproval;
  busy: boolean;
  error?: string | null;
  draft?: MediaApprovalDraftController;
  onRespond(response: OpenCodeMediaApprovalRespondRequest['response'], items?: OpenCodeMediaApprovalItem[]): Promise<boolean>;
}

const sourceNames: Record<string, string> = {
  'openai-subscription': 'ChatGPT subscription', 'glowbom-api': 'Glowbom account', 'openai-api': 'OpenAI API',
  'gemini-api': 'Google API', 'xai-api': 'SpaceXAI API', 'xai-subscription': 'Grok subscription',
  'veo-api': 'Google API', 'elevenlabs-api': 'ElevenLabs API',
};

function MediaGlyph({ kind }: { kind: string }) {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    {kind === 'video' ? <><rect x="3" y="5" width="13" height="14" rx="3" /><path d="m16 10 5-3v10l-5-3" /></>
      : kind === 'audio' ? <><path d="M5 10v4m4-7v10m4-13v16m4-13v10m4-7v4" /></>
        : <><rect x="3" y="3" width="18" height="18" rx="4" /><circle cx="8" cy="8" r="1.5" /><path d="m3 17 5-5 4 4 4-6 5 7" /></>}
  </svg>;
}

export function MediaApprovalEditor({ approval, busy, error, onRespond, draft }: Props) {
  const ownDraft = useMediaApprovalDraft(approval);
  const { items, setItems } = draft || ownDraft;
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [addKind, setAddKind] = useState<MediaKind>('image');
  const [sources, setSources] = useState<ProjectIconSource[]>([]);
  const [imageSources, setImageSources] = useState<StudioImageSource[]>([...studioImageSources]);
  const [sourcesLoaded, setSourcesLoaded] = useState(false);
  const [loadingSources, setLoadingSources] = useState(true);
  const [videoSources, setVideoSources] = useState<StudioVideoSource[]>([]);
  const [loadingVideoSources, setLoadingVideoSources] = useState(true);
  const [videoSourceError, setVideoSourceError] = useState('');
  const [sourceError, setSourceError] = useState('');
  const [localError, setLocalError] = useState('');
  const [referenceBusy, setReferenceBusy] = useState('');
  const [sourceRefresh, setSourceRefresh] = useState(0);
  const [, setKeyRefresh] = useState(0);
  const addButtonRef = useRef<HTMLButtonElement>(null);
  const listId = useId();
  const keys = readIconApiKeys();
  const selected = items.filter((item) => !item.excluded);
  const imageSelected = selected.some((item) => item.mediaType === 'image');
  const videoSelected = selected.some((item) => item.mediaType === 'video');
  const imageConnections = mediaApprovalImageConnections(items, sources);
  const validationError = mediaApprovalGenerateError(items);
  const unavailableSource = items.map((item) => mediaApprovalSourceError(item, sources, keys, sourcesLoaded, loadingVideoSources ? undefined : videoSources)).find(Boolean);
  const generateDisabled = busy || !!referenceBusy || !!validationError || !!unavailableSource || (imageSelected && (loadingSources || !!sourceError)) || (videoSelected && (loadingVideoSources || !!videoSourceError));
  const videoEstimates = selected.filter((item) => item.mediaType === 'video').map((item) => studioVideoEstimate(mediaApprovalVideoOptions(item), true));
  const videoTotalUSD = videoEstimates.reduce((total, estimate) => total + (estimate.usd || 0), 0);
  const hasSubscriptionVideo = selected.some((item) => item.mediaType === 'video' && item.sourceId === 'xai-subscription');
  const imageEstimates = selected.filter((item) => item.mediaType === 'image').map((item) => studioImageEstimate(mediaApprovalImageOptions(item), !!item.referenceImages?.length, imageSources));
  const imageTotalUSD = imageEstimates.reduce((total, estimate) => total + (estimate.usd || 0), 0);
  const hasAccountImages = selected.some((item) => item.mediaType === 'image' && !studioProviderKeySource(item.sourceId || ''));
  const hasUnpricedImages = selected.some((item) => item.mediaType === 'image' && !!studioProviderKeySource(item.sourceId || '') && studioImageEstimate(mediaApprovalImageOptions(item), !!item.referenceImages?.length, imageSources).usd === undefined);

  useEffect(() => { setExpandedId(null); setLocalError(''); }, [approval.id]);
  useEffect(() => {
    const refresh = () => setSourceRefresh((current) => current + 1);
    const refreshKeys = () => { setKeyRefresh((current) => current + 1); refresh(); };
    window.addEventListener(imageSettingsChangedEvent, refreshKeys);
    window.addEventListener(accountStatusChangedEvent, refresh);
    return () => {
      window.removeEventListener(imageSettingsChangedEvent, refreshKeys);
      window.removeEventListener(accountStatusChangedEvent, refresh);
    };
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    setLoadingSources(true);
    setSourceError('');
    void loadMediaApprovalImageSources(controller.signal).then((loaded) => {
      if (controller.signal.aborted) return;
      setImageSources(loaded.imageSources);
      setSources(loaded.sources);
      setSourcesLoaded(true);
    }).catch((requestError: unknown) => {
      if (!controller.signal.aborted) setSourceError(toErrorMessage(requestError, 'Could not load image sources. Refresh sources to try again.'));
    }).finally(() => { if (!controller.signal.aborted) setLoadingSources(false); });
    return () => controller.abort();
  }, [sourceRefresh]);

  useEffect(() => {
    if (!videoSelected) { setLoadingVideoSources(false); return; }
    const controller = new AbortController();
    setLoadingVideoSources(true);
    setVideoSourceError('');
    void loadMediaApprovalVideoSources(controller.signal).then((available) => {
      if (!controller.signal.aborted) setVideoSources(available);
    }).catch((requestError: unknown) => {
      if (!controller.signal.aborted) setVideoSourceError(toErrorMessage(requestError, 'Could not load video sources. Refresh sources to try again.'));
    }).finally(() => { if (!controller.signal.aborted) setLoadingVideoSources(false); });
    return () => controller.abort();
  }, [sourceRefresh, videoSelected]);

  function updateItem(id: string, patch: Partial<OpenCodeMediaApprovalItem>) {
    setLocalError('');
    setItems((current) => current.map((item) => item.id === id ? { ...item, ...patch } : item));
  }

  async function attachReference(id: string, file: File) {
    setReferenceBusy(id);
    setLocalError('');
    try {
      const reference = await readIconReference(file);
      updateItem(id, mediaApprovalReferencePatch(reference.image));
    } catch (referenceError) {
      setLocalError(toErrorMessage(referenceError, 'Could not read this reference image.'));
    } finally { setReferenceBusy(''); }
  }

  async function respond(response: OpenCodeMediaApprovalRespondRequest['response']) {
    if (busy || referenceBusy) return;
    if (response === 'generate' && generateDisabled) return;
    setLocalError('');
    try {
      const sent = await onRespond(response, response === 'generate' ? mediaApprovalDraft(items) : undefined);
      if (!sent) setLocalError('Could not send your choice. Your asset edits are saved here. Try again.');
    } catch (requestError) {
      setLocalError(toErrorMessage(requestError, 'Could not send your choice. Your asset edits are saved here. Try again.'));
    }
  }

  return <form className="work-ask media-approval-editor" onSubmit={(event) => { event.preventDefault(); void respond('generate'); }}>
    <div className="work-ask-content" role="region" aria-label="Media request details" tabIndex={0}>
      <div className="media-editor-intro"><h2>{approval.title || 'Create these assets?'}</h2>
        {approval.message && <p className="media-editor-note">{approval.message}</p>}
        <p className="media-editor-note">Open an asset to adjust its source, prompt, duration, or other settings.</p>
      </div>
      <div className="media-editor-toolbar"><span>{selected.length} of {items.length} assets selected</span></div>
      {videoSelected && <p className="media-editor-note">Video estimate: {videoTotalUSD > 0 ? `about $${videoTotalUSD.toFixed(2)} USD` : 'account usage'}{hasSubscriptionVideo && videoTotalUSD > 0 ? ', plus Grok account usage' : ''}. Images and audio are charged separately. Actual provider charges may differ.</p>}
      {imageSelected && <p className="media-editor-note">Image output estimate: {imageTotalUSD > 0 ? `about $${imageTotalUSD.toFixed(3)} USD` : hasUnpricedImages ? 'choose valid settings to estimate cost' : 'account usage'}{hasAccountImages && imageTotalUSD > 0 ? ', plus account usage' : ''}{hasUnpricedImages && imageTotalUSD > 0 ? ', with other image costs unavailable' : ''}. Prompt, reference, or thinking tokens can add to the total.</p>}
      {sourceError && imageSelected && <p className="media-editor-error" role="alert">{sourceError}</p>}
      {videoSourceError && videoSelected && <p className="media-editor-error" role="alert">{videoSourceError}</p>}
      <div className="media-editor-list">
        {items.map((item, index) => {
          const added = item.id.startsWith('added-');
          const selectedSource = sources.find((source) => source.id === item.sourceId);
          const excludedStartingImage = !item.excluded && item.mediaType === 'video' && items.some((image) => image.mediaType === 'image' && image.excluded && [image.id, image.prompt, image.placeholder].includes(item.fromKey));
          const itemError = mediaApprovalItemError(item) || mediaApprovalSourceError(item, sources, keys, sourcesLoaded, loadingVideoSources ? undefined : videoSources) || (excludedStartingImage ? 'Include the video’s starting image or choose another starting frame.' : '');
          const expanded = expandedId === item.id;
          const panelId = `${listId}-${item.id}`;
          const videoOptions = mediaApprovalVideoOptions(item);
          const videoModel = studioVideoModel(videoOptions.sourceId, videoOptions.modelId);
          const videoEstimate = studioVideoEstimate(videoOptions, true);
          const imageOptions = mediaApprovalImageOptions(item);
          const imageModel = studioImageModel(imageOptions.sourceId, imageOptions.modelId, imageSources);
          const imageEstimate = studioImageEstimate(imageOptions, !!item.referenceImages?.length, imageSources);
          const audioMode = ['voice', 'sound', 'music'].includes(item.audioType || '') ? item.audioType as StudioAudioMode : undefined;
          const sourceName = item.mediaType === 'video' ? `${studioProviderName(videoOptions.sourceId)} · ${videoModel.name}` : item.mediaType === 'image' && imageModel ? `${studioProviderName(imageOptions.sourceId)} · ${imageModel.name}` : sourceNames[item.sourceId || ''] || (selectedSource ? imageSourceName(selectedSource) : 'Choose a source');
          const reference = item.referenceImages?.[0];
          const hasReference = !!reference;
          const previousReference = hasReference && item.referenceOrigin === 'previous-image';
          return <article className={`media-editor-item${item.excluded ? ' media-editor-excluded' : ''}${expanded ? ' media-editor-expanded' : ''}`} key={item.id}>
            <div className="media-editor-heading">
              <label className="media-editor-include" title={item.excluded ? 'Include this asset' : 'Exclude this asset'}><input type="checkbox" aria-label={`Include asset ${index + 1}`} aria-describedby={`${panelId}-title`} checked={!item.excluded} disabled={busy || !!referenceBusy} onChange={(event) => updateItem(item.id, { excluded: !event.target.checked })} /></label>
              <button type="button" className="media-editor-disclosure" aria-label={`Edit asset ${index + 1}`} aria-describedby={`${panelId}-title ${panelId}-meta`} aria-expanded={expanded} aria-controls={panelId} onClick={() => setExpandedId(expanded ? null : item.id)}>
                <span className="media-editor-glyph"><MediaGlyph kind={item.mediaType} /></span>
                <span className="media-editor-summary"><span id={`${panelId}-title`} className="media-editor-title">{item.prompt.trim() || `New ${item.mediaType}`}</span>
                  <span id={`${panelId}-meta`} className="media-editor-meta"><span className="media-editor-kind">{item.audioType === 'voice' ? 'Voice' : item.audioType === 'sound' ? 'Sound effect' : item.audioType === 'music' ? 'Music' : item.mediaType}</span><span>{sourceName}</span>{item.aspectRatio && <span>{item.aspectRatio}</span>}{['audio', 'video'].includes(item.mediaType) && item.durationSeconds !== undefined && item.durationSeconds > 0 && <span>{item.durationSeconds} seconds</span>}{item.mediaType === 'video' && <><span>{item.resolution}</span><span>{item.sourceId === 'xai-subscription' ? 'Grok account usage' : videoEstimate.usd !== undefined ? `About $${videoEstimate.usd.toFixed(2)}` : 'Estimate unavailable'}</span></>}{item.audioType === 'music' && item.forceInstrumental && <span>Instrumental</span>}{item.mediaType === 'image' && <><span>{imageOptions.resolution}</span>{imageOptions.quality && <span>{imageOptions.quality}</span>}<span>{imageEstimate.label}</span><span className="media-editor-reference-status">{previousReference ? 'Previous image' : hasReference ? '1 reference' : 'No reference'}</span></>}{added && <span>Added</span>}{item.excluded ? <span className="media-editor-state">Excluded</span> : itemError && <span className="media-editor-attention">Needs attention</span>}</span>
                </span>
                <svg className="media-editor-chevron" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="m6 3 5 5-5 5" /></svg>
              </button>
            </div>
            <div id={panelId} className="media-editor-details" hidden={!expanded}>
            <fieldset disabled={busy || !!item.excluded || !!referenceBusy}>
              <div className="media-editor-source-settings">
              {item.mediaType === 'image' ? <StudioImageSettings options={imageOptions} sources={imageSources} hideKey loading={loadingSources} disabled={busy || !!item.excluded || !!referenceBusy} hasReference={hasReference} onChange={(options) => updateItem(item.id, updateMediaImageOptions(options))} /> : <label>Generate with<select value={item.sourceId || ''} onChange={(event) => updateItem(item.id, item.mediaType === 'video' ? updateMediaVideoOptions(item, { sourceId: event.target.value as StudioVideoSourceID, modelId: '', resolution: '', durationSeconds: 0 }) : { sourceId: event.target.value })}>
                <option value="">Choose a source</option>
                {item.sourceId && !['veo-api', 'xai-api', 'xai-subscription', 'elevenlabs-api'].includes(item.sourceId) && <option value={item.sourceId}>{item.sourceId}</option>}
                {item.mediaType === 'video' && studioVideoSources.map((source) => <option key={source.id} value={source.id}>{source.name}</option>)}
                {item.mediaType === 'audio' && <option value="elevenlabs-api">ElevenLabs API</option>}
              </select></label>}
              {item.mediaType === 'video' && <>
                <label>Model<select value={videoOptions.modelId} onChange={(event) => updateItem(item.id, updateMediaVideoOptions(item, { modelId: event.target.value }))}>
                  {(videoSources.find((source) => source.id === item.sourceId)?.models || []).map((model) => <option key={model.id} value={model.id}>{model.name}</option>)}
                </select></label>
                <label>Length<select value={videoOptions.durationSeconds} onChange={(event) => updateItem(item.id, { durationSeconds: Number(event.target.value) })}>{studioVideoDurations(videoOptions).map((seconds) => <option key={seconds} value={seconds}>{seconds} {seconds === 1 ? 'second' : 'seconds'}</option>)}</select></label>
                <label>Resolution<select value={videoOptions.resolution} onChange={(event) => updateItem(item.id, updateMediaVideoOptions(item, { resolution: event.target.value }))}>{videoModel.resolutions.map((resolution) => <option key={resolution} value={resolution}>{resolution}</option>)}</select></label>
                <label>Shape<select value={videoOptions.aspectRatio} onChange={(event) => updateItem(item.id, { aspectRatio: event.target.value })}>{videoModel.aspectRatios.map((ratio) => <option key={ratio} value={ratio}>{ratio === '16:9' ? 'Landscape · 16:9' : ratio === '9:16' ? 'Portrait · 9:16' : 'Square · 1:1'}</option>)}</select></label>
              </>}
              </div>
              {selectedSource && <p className="media-editor-note">{imageSourceHelp(selectedSource)}</p>}
              {item.mediaType === 'video' && <>
                <p className="media-editor-note">{videoEstimate.label}. <a href={videoEstimate.pricingURL} target="_blank" rel="noreferrer">Provider pricing</a>, checked {videoEstimate.checkedOn}. Actual charges may differ.</p>
                {item.sourceId === 'veo-api' && item.resolution === '1080p' && <p className="media-editor-note">Google requires 8 seconds at 1080p.</p>}
                {item.sourceId === 'xai-subscription' && <p className="media-editor-note">Uses your Grok connection in OpenCode. Video access depends on your account; allowance or purchased credits may apply.</p>}
              </>}
              {item.mediaType === 'audio' && <p className="media-editor-note">Uses your ElevenLabs API key in Settings. Provider charges apply.</p>}
              <label>{item.audioType === 'voice' ? 'Words to speak' : 'Generation prompt'}<textarea rows={3} maxLength={10000} autoFocus={added && expanded} value={item.prompt} onChange={(event) => updateItem(item.id, { prompt: event.target.value })} placeholder="Describe what to create" /></label>
              {item.mediaType === 'image' && <>
                {item.sourceId === 'glowbom-api' && <p className="media-editor-note">Glowbom chooses the image size.{item.aspectRatio && <button type="button" className="button secondary" onClick={() => updateItem(item.id, { aspectRatio: '' })}>Use Glowbom sizing</button>}</p>}
                <div className="media-editor-reference-section"><span className="media-editor-label">Reference image <span className="media-editor-note">Optional</span></span>
                  <p className="media-editor-note">{previousReference ? 'Using the previous image to guide this version. Replace it or remove it to start fresh.' : 'Guide the image with a photo. PNG, JPEG, or WebP.'}</p>
                  <label className="media-editor-upload"><input type="file" accept="image/png,image/jpeg,image/webp" aria-label={`Reference for asset ${index + 1}`} onChange={(event) => { const file = event.currentTarget.files?.[0]; event.currentTarget.value = ''; if (file) void attachReference(item.id, file); }} /><span>{hasReference ? 'Replace reference' : 'Add reference'}</span></label>
                {referenceBusy === item.id && <p role="status">Reading reference…</p>}
                {(item.referenceImages || []).map((reference, referenceIndex) => <div className="media-editor-reference" key={referenceIndex}>{/^data:image\/(?:png|jpeg|webp);base64,/.test(reference) && (previousReference ? <figure style={{ margin: 0 }}><img src={reference} alt={`Reference for asset ${index + 1}`} /><figcaption className="media-editor-note">Previous image attached</figcaption></figure> : <img src={reference} alt={`Reference for asset ${index + 1}`} />)}<button type="button" className="button secondary" onClick={() => updateItem(item.id, mediaApprovalReferencePatch())}>Remove reference</button></div>)}
                </div>
              </>}
              {item.mediaType === 'video' && <>
                <label>Starting image<input list={`${listId}-images`} value={items.find((image) => image.mediaType === 'image' && image.id === item.fromKey)?.prompt || item.fromKey || ''} onChange={(event) => updateItem(item.id, { fromKey: items.find((image) => image.mediaType === 'image' && image.prompt === event.target.value)?.id || event.target.value })} placeholder="Image prompt or saved image ID" /></label>
              </>}
              {item.mediaType === 'audio' && <>
                <label>Audio type<select value={item.audioType || ''} onChange={(event) => { const audioType = event.target.value as 'voice' | 'sound' | 'music'; updateItem(item.id, updateMediaAudioType(item, audioType)); }}><option value="" disabled>Choose audio type</option><option value="voice">Voice</option><option value="sound">Sound effect</option><option value="music">Music</option></select></label>
                {item.audioType === 'voice' && <label>Voice ID<input maxLength={200} value={item.voiceID || ''} onChange={(event) => updateItem(item.id, { voiceID: event.target.value })} placeholder="Use account default voice" /></label>}
                {audioMode && <ElevenLabsModelSelector mode={audioMode} model={item.modelID ?? defaultStudioAudioDraft()[audioMode].model} onChange={(modelID) => updateItem(item.id, { modelID, ...(item.audioType === 'sound' && modelID !== 'eleven_text_to_sound_v2' ? { loop: false } : {}) })} />}
                {(item.audioType === 'sound' || item.audioType === 'music') && <label>Duration in seconds<input type="number" min={item.audioType === 'music' ? 3 : 0.5} max={item.audioType === 'music' ? 600 : 30} step={0.5} required={item.audioType === 'music'} value={item.durationSeconds ?? ''} onChange={(event) => updateItem(item.id, { durationSeconds: event.currentTarget.value === '' ? undefined : event.currentTarget.valueAsNumber })} placeholder={item.audioType === 'music' ? 'Choose a duration' : 'Automatic'} /></label>}
                {item.audioType === 'music' && <p className="media-editor-note">Music starts at 30 seconds. Choose the length before generating; longer tracks can use more credits.</p>}
                {item.audioType === 'sound' && <><label>Prompt influence<input type="number" min={0} max={1} step={0.05} value={item.promptInfluence ?? ''} onChange={(event) => updateItem(item.id, { promptInfluence: event.currentTarget.value === '' ? undefined : event.currentTarget.valueAsNumber })} placeholder="Provider default" /></label><label className="media-editor-check"><input type="checkbox" checked={!!item.loop} onChange={(event) => updateItem(item.id, { loop: event.target.checked })} />Loop this sound</label></>}
                {item.audioType === 'music' && <label className="media-editor-check"><input type="checkbox" checked={!!item.forceInstrumental} onChange={(event) => updateItem(item.id, { forceInstrumental: event.target.checked })} />Instrumental only</label>}
              </>}
              <label>How the app should use it{added && <span className="media-editor-note">Required for added assets</span>}<textarea rows={2} maxLength={10000} value={item.usagePrompt || ''} onChange={(event) => updateItem(item.id, { usagePrompt: event.target.value })} placeholder={added ? 'For example: Use this as the home page background' : 'Optional placement or behavior instructions'} /></label>
            </fieldset>
            {itemError && <p className="media-editor-error">{itemError}</p>}
            {added && <button type="button" className="button secondary" disabled={busy || !!referenceBusy} onClick={() => { setItems((current) => current.filter((candidate) => candidate.id !== item.id)); setExpandedId(null); addButtonRef.current?.focus(); }}>Remove added asset</button>}
            </div>
          </article>;
        })}
      </div>
      <datalist id={`${listId}-images`}>{items.filter((item) => item.mediaType === 'image' && !item.excluded && item.prompt).map((item) => <option key={item.id} value={item.prompt} />)}</datalist>
      <div className="media-editor-add"><label>New asset type<select value={addKind} disabled={busy || !!referenceBusy} onChange={(event) => setAddKind(event.target.value as MediaKind)}><option value="image">Image</option><option value="video">Video</option><option value="audio">Audio</option></select></label><button ref={addButtonRef} type="button" className="button secondary" disabled={busy || !!referenceBusy || items.length >= 100} onClick={() => { const item = newMediaApprovalItem(addKind); setLocalError(''); setItems((current) => [...current, item]); setExpandedId(item.id); }}>Add asset</button></div>
      <div className="media-editor-connection-tools">
        {imageConnections.length > 0 && <details className="media-editor-connections">
          <summary>Media connections</summary>
          <p className="media-editor-note">Optional API keys for this session, shared by icons and Build. Empty fields use your saved connection. Provider charges apply.</p>
          {imageConnections.map((source) => <label key={source.id}>{studioProviderKeyLabel(source.id)}<input type="password" autoComplete="off" spellCheck={false} value={readImageSessionKey(source.id)} disabled={busy || !!referenceBusy} onChange={(event) => { setLocalError(''); rememberImageSessionKey(source.id, event.target.value); }} placeholder={studioProviderCredentials(source.id).useSavedKey || keys[source.id] ? 'Saved connection configured' : 'Add a key for this session'} /></label>)}
        </details>}
        <button type="button" className="media-editor-refresh" disabled={busy || loadingSources || loadingVideoSources} onClick={() => setSourceRefresh((current) => current + 1)}>{loadingSources || loadingVideoSources ? 'Checking sources…' : 'Refresh sources'}</button>
      </div>
      {validationError && <p className="media-editor-note">{validationError}</p>}
      {(error || localError) && <p className="media-editor-error" role="alert">{error || localError}</p>}
    </div>
    <div className="work-actions work-ask-actions">
      <button type="submit" className="button" disabled={generateDisabled}>{busy ? 'Sending…' : `Create selected (${selected.length})`}</button>
      <button type="button" className="button secondary" disabled={busy || !!referenceBusy} onClick={() => { void respond('skip'); }}>Skip all</button>
    </div>
  </form>;
}
