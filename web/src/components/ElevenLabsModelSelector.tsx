import { useId } from 'react';
import { studioAudioModels, type StudioAudioMode } from '../lib/studio-audio';
import './elevenlabs-model-selector.css';

export interface ElevenLabsModelSelectorProps { mode: StudioAudioMode; model: string; onChange(model: string): void; disabled?: boolean }
export function ElevenLabsModelSelector({ mode, model, onChange, disabled = false }: ElevenLabsModelSelectorProps) {
  const id = useId(), models = studioAudioModels[mode], custom = !models.some((candidate) => candidate.id === model);
  return <div className="elevenlabs-model-selector">
    <label htmlFor={`${id}-model`}>ElevenLabs model</label><select id={`${id}-model`} value={custom ? 'custom' : model} disabled={disabled} onChange={(event) => onChange(event.target.value === 'custom' ? '' : event.target.value)}>
      {models.map((candidate) => <option key={candidate.id} value={candidate.id}>{candidate.name}</option>)}<option value="custom">Custom model ID</option>
    </select>
    {custom && <><label htmlFor={`${id}-custom`}>Custom model ID</label><input id={`${id}-custom`} value={model} disabled={disabled} placeholder="Model ID from ElevenLabs" maxLength={200} onChange={(event) => onChange(event.target.value)} /><p className="studio-generator-note">Your account must support this model for {mode === 'voice' ? 'speech' : mode === 'music' ? 'music' : 'sound effects'}. The provider may reject an unsupported ID.</p></>}
    {mode === 'voice' && model === 'eleven_v4' && <p className="studio-generator-note">Eleven v4 uses your chosen voice through dialogue generation. Short passages work best.</p>}
  </div>;
}
