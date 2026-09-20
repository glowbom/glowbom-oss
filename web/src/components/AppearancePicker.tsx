import { useEffect, useState } from 'react';
import { applyAppearance, readAppearance, saveAppearance, type Appearance } from '../lib/appearance';

export function useAppearance() {
  const [appearance, setAppearance] = useState(readAppearance);
  useEffect(() => {
    const media = window.matchMedia('(prefers-color-scheme: dark)');
    const update = () => applyAppearance(appearance, media.matches);
    update();
    media.addEventListener('change', update);
    return () => media.removeEventListener('change', update);
  }, [appearance]);
  return { appearance, onAppearanceChange: (value: Appearance) => { saveAppearance(value); setAppearance(value); } };
}

export interface AppearanceProps {
  appearance: Appearance;
  onAppearanceChange: (value: Appearance) => void;
}

export function AppearancePicker({ appearance, onAppearanceChange }: AppearanceProps) {
  return <label className="appearance-picker">
    <span>Appearance</span>
    <select value={appearance} onChange={(event) => onAppearanceChange(event.target.value as Appearance)}>
      <option value="system">System</option><option value="light">Light</option><option value="dark">Dark</option>
    </select>
  </label>;
}
