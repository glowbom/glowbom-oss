export type Appearance = 'system' | 'light' | 'dark';
const key = 'glowbom_oss_appearance';

export function parseAppearance(value: string | null): Appearance {
  return value === 'light' || value === 'dark' ? value : 'system';
}

export function readAppearance(): Appearance {
  try { return parseAppearance(localStorage.getItem(key)); } catch { return 'system'; }
}

export function saveAppearance(value: Appearance) {
  try { localStorage.setItem(key, value); } catch { /* Keep the choice for this session. */ }
}

export function applyAppearance(value: Appearance, prefersDark: boolean) {
  document.documentElement.dataset.theme = value === 'system' ? (prefersDark ? 'dark' : 'light') : value;
}
