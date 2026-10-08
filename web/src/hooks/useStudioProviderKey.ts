import { useCallback, useEffect, useState } from 'react';
import { imageSettingsChangedEvent, readIconApiKeys, readImageSessionKey } from '../lib/project-icon';
import { loadStudioProviderKey, rememberStudioProviderKey, removeStudioProviderKey, saveStudioProviderKey, studioProviderKeyIsSaved, studioProviderKeySource } from '../lib/studio-providers';

export function useStudioProviderKey(sourceId: string, connected = false) {
  const provider = studioProviderKeySource(sourceId);
  const [revision, setRevision] = useState(0), [savedKey, setSavedKey] = useState(false), [configured, setConfigured] = useState(false);
  const [loading, setLoading] = useState(false), [saving, setSaving] = useState(false), [keyError, setKeyError] = useState('');
  const [, setSessionRevision] = useState(0);
  const refresh = useCallback(() => setRevision((value) => value + 1), []);
  useEffect(() => {
    const refresh = () => { setSavedKey(studioProviderKeyIsSaved(sourceId)); setSessionRevision((value) => value + 1); };
    window.addEventListener(imageSettingsChangedEvent, refresh);
    return () => window.removeEventListener(imageSettingsChangedEvent, refresh);
  }, [sourceId]);
  useEffect(() => {
    setSavedKey(false); setConfigured(false); setKeyError('');
    if (!provider) { setLoading(false); return; }
    const controller = new AbortController(); setLoading(true);
    void loadStudioProviderKey(provider, controller.signal).then((status) => {
      if (!controller.signal.aborted) { setSavedKey(status.saved); setConfigured(status.configured); }
    }).catch(() => { /* A configured or session key remains usable if secure storage is unavailable. */ })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [provider, revision]);
  const inputKey = provider ? readImageSessionKey(provider) : '';
  const ready = !provider || !!inputKey.trim() || savedKey || configured || connected || !!readIconApiKeys()[provider];
  async function saveKey() {
    if (!inputKey.trim() || saving) return;
    setSaving(true); setKeyError('');
    try { await saveStudioProviderKey(sourceId, inputKey); setSavedKey(true); setConfigured(true); }
    catch (cause) { setKeyError(cause instanceof Error ? cause.message : 'Could not save this key securely.'); }
    finally { setSaving(false); }
  }
  async function removeKey() {
    if (saving) return;
    setSaving(true); setKeyError('');
    try { await removeStudioProviderKey(sourceId); setSavedKey(false); setConfigured(false); }
    catch (cause) { setKeyError(cause instanceof Error ? cause.message : 'Could not remove this saved key.'); }
    finally { setSaving(false); }
  }
  return { ready, inputKey, savedKey, configured, loading, saving, keyError, saveKey, removeKey,
    refresh, changeKey: (key: string) => { rememberStudioProviderKey(sourceId, key); setKeyError(''); },
  };
}
