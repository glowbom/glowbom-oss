import { studioProviderKeyLabel } from '../lib/studio-providers';
import type { useStudioProviderKey } from '../hooks/useStudioProviderKey';
import './studio-provider-key.css';

export function StudioProviderKey({ sourceId, settings, connected = false, disabled = false }: {
  sourceId: string; settings: Pick<ReturnType<typeof useStudioProviderKey>, 'inputKey' | 'savedKey' | 'saving' | 'keyError' | 'changeKey' | 'saveKey' | 'removeKey'>; connected?: boolean; disabled?: boolean;
}) {
  return <div className="studio-provider-key">
    <label>{studioProviderKeyLabel(sourceId)}<input type="password" autoComplete="off" spellCheck={false} value={settings.inputKey} disabled={disabled || settings.saving} placeholder={settings.savedKey ? 'Saved securely. Enter a key to replace it.' : connected ? 'A configured key is available' : 'Enter your API key'} onChange={(event) => settings.changeKey(event.target.value)} /></label>
    <div className="studio-provider-key-actions"><button type="button" disabled={disabled || settings.saving || !settings.inputKey.trim()} onClick={() => void settings.saveKey()}>{settings.saving ? 'Saving…' : 'Save key'}</button>{settings.savedKey && <button type="button" disabled={disabled || settings.saving} onClick={() => void settings.removeKey()}>Remove saved key</button>}</div>
    <p className="studio-generator-note">Saved keys use the system credential store and are shared across image and video generation. An unsaved key lasts for this session and is shared with Draw, app icons, and Build.</p>
    {settings.keyError && <p className="studio-provider-key-error" role="alert">{settings.keyError}</p>}
  </div>;
}
