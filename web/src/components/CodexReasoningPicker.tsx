import { useEffect, useLayoutEffect, useRef, useState, type CSSProperties } from 'react';
import { createPortal } from 'react-dom';
import type { ChatModel } from '../lib/chat';
import { codexReasoningEfforts, reasoningEffortLabel, selectedCodexReasoningEffort } from '../lib/codex-reasoning';
import './codex-reasoning.css';

export function CodexReasoningPicker({ model, value, disabled = false, onChange }: { model?: ChatModel; value?: string; disabled?: boolean; onChange(effort: string): void }) {
  const [open, setOpen] = useState(false);
  const [position, setPosition] = useState<CSSProperties>({ visibility: 'hidden' });
  const trigger = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const slider = useRef<HTMLInputElement>(null);
  const efforts = codexReasoningEfforts(model);
  const selected = model ? selectedCodexReasoningEffort(model, value ? { [model.id]: value } : {}) : undefined;
  const fallback = selectedCodexReasoningEffort(model, {});
  const index = Math.max(0, efforts.indexOf(selected || ''));
  function close(restoreFocus = false) { setOpen(false); if (restoreFocus) trigger.current?.focus(); }

  useEffect(() => { if (disabled || !efforts.length) close(); }, [disabled, model?.id, efforts.join(',')]);
  useEffect(() => {
    if (!open) return;
    const outside = (event: Event) => {
      const target = event.target as Node;
      if (!trigger.current?.contains(target) && !panel.current?.contains(target)) close();
    };
    document.addEventListener('pointerdown', outside);
    document.addEventListener('focusin', outside);
    return () => { document.removeEventListener('pointerdown', outside); document.removeEventListener('focusin', outside); };
  }, [open]);
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const rect = trigger.current?.getBoundingClientRect();
      if (!rect) return;
      const width = Math.min(280, window.innerWidth - 16);
      const height = panel.current?.offsetHeight || 118;
      setPosition({ width, left: Math.max(8, Math.min(rect.right - width, window.innerWidth - width - 8)), top: rect.top >= height + 16 ? rect.top - height - 8 : Math.min(rect.bottom + 8, window.innerHeight - height - 8) });
    };
    place();
    window.addEventListener('resize', place); window.addEventListener('scroll', place, true);
    return () => { window.removeEventListener('resize', place); window.removeEventListener('scroll', place, true); };
  }, [open]);

  useEffect(() => { if (open && position.visibility !== 'hidden') slider.current?.focus(); }, [open, position.visibility]);

  if (!model || !selected || !efforts.length) return null;
  const label = reasoningEffortLabel(selected);
  return <span className="codex-reasoning" data-effort={selected}>
    <button ref={trigger} type="button" className="codex-reasoning-trigger" aria-label={`Codex reasoning effort: ${label}`} aria-haspopup="dialog" aria-expanded={open} disabled={disabled} onClick={() => setOpen(current => !current)}>{label}</button>
    {open && typeof document !== 'undefined' && createPortal(<div ref={panel} className="codex-reasoning-panel" data-effort={selected} style={position} role="dialog" aria-label="Codex reasoning effort" onKeyDown={event => { if (event.key === 'Escape') { event.preventDefault(); close(true); } }}>
      <div className="codex-reasoning-heading">
        <div><span>Thinking effort</span><strong>{label}</strong></div>
        <button type="button" className="codex-reasoning-reset" aria-label="Reset reasoning effort" title={`Reset to ${reasoningEffortLabel(fallback || selected)}`} disabled={selected === fallback} onClick={() => { if (fallback) onChange(fallback); }}>Reset</button>
      </div>
      <input ref={slider} className="codex-reasoning-slider" style={{ '--codex-effort-progress': `${efforts.length > 1 ? index / (efforts.length - 1) * 100 : 100}%` } as CSSProperties} type="range" min={0} max={efforts.length - 1} step={1} value={index} aria-label="Reasoning effort" aria-valuetext={label} disabled={efforts.length < 2} onChange={event => { const effort = efforts[Number(event.target.value)]; if (effort) onChange(effort); }} />
      <div className="codex-reasoning-model">{model.name}</div>
    </div>, trigger.current?.closest('dialog') || document.body)}
  </span>;
}
