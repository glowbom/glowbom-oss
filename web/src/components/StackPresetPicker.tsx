import type { DiscoveredStack } from '../lib/preview';
import { useState } from 'react';
import { STACK_CATEGORIES, STACK_PRESETS, type StackCategory, type StackPreset } from '../lib/stack-presets';

export function StackPresetPicker({ selected, disabled, onChoose, existing = [] }: {
  existing?: DiscoveredStack[]; selected: string; disabled: boolean; onChoose: (preset?: StackPreset) => void;
}) {
  const [category, setCategory] = useState<StackCategory>('web');
  const [search, setSearch] = useState('');
  const query = search.trim().toLowerCase();
  const choices = STACK_PRESETS.filter((p) => query
    ? `${p.name} ${p.summary} ${p.requirements}`.toLowerCase().includes(query)
    : p.category === category);
  return <div className="stack-picker">
    <div className="stack-picker-heading">
      <label>Find a stack<input className="input" type="search" value={search} onChange={(e) => setSearch(e.target.value)} placeholder="Search React, PHP, games…" /></label>
      <button type="button" className={`preview-tab ${!selected ? 'selected' : ''}`} aria-pressed={!selected} disabled={disabled} onClick={() => onChoose()}>Describe your own</button>
    </div>
    <div className="preview-tabs" role="group" aria-label="Stack categories">
      {STACK_CATEGORIES.map((c) => <button type="button" className={`preview-tab ${!query && category === c.id ? 'selected' : ''}`} aria-pressed={!query && category === c.id} disabled={disabled} key={c.id} onClick={() => { setCategory(c.id); setSearch(''); }}>{c.name}</button>)}
    </div>
    <div className="stack-preset-grid" role="group" aria-label="Stack presets">
      {choices.map((p) => <button type="button" className={`preview-tab stack-preset ${selected === p.id ? 'selected' : ''}`} aria-pressed={selected === p.id} disabled={disabled} key={p.id} onClick={() => onChoose(p)}>
        <strong>{p.name}</strong>{existing.some((app) => app.preset === p.id) ? <span className="stack-badge">Already in project</span> : null}<span>{p.summary}</span><span className="stack-capability">{p.preview}</span><span>Requires: {p.requirements}</span>
      </button>)}
    </div>
    {!choices.length ? <p className="meta">No matching preset. Describe your own stack to continue.</p> : null}
    <p className="meta">Presets provide build instructions. Preview becomes available after the app and its required tools are ready.</p>
  </div>;
}
