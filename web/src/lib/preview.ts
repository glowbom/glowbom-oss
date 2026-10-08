import { withServerAuthHeaders } from './server-auth';

export interface PreviewDefinition {
  id: string;
  name: string;
  directory: string;
  command?: string[];
  description?: string;
  preset?: string;
  previewMode?: 'auto' | 'command' | 'none';
  previewNotes?: string;
}

export interface PreviewTarget {
  target: string;
  name: string;
  directory: string;
  command?: string[];
  description?: string;
  preset?: string;
  previewMode?: 'auto' | 'command' | 'none';
  previewNotes?: string;
  canOpenTerminal?: boolean;
  canOpenFolder?: boolean;
  kind: 'static' | 'next' | 'vite' | 'custom' | '';
  available: boolean;
  needsInstall: boolean;
  reason?: string;
  status: 'stopped' | 'installing' | 'starting' | 'running' | 'failed';
  id?: string;
  url?: string;
  error?: string;
  logs?: string[];
  revision?: string;
}

export function resultPreviewTarget(targets: PreviewTarget[]): PreviewTarget | undefined {
  const canShow = (target: PreviewTarget) => stackIsInProject(target) && target.previewMode !== 'none' && (target.available || !!target.url);
  return targets.find((target) => target.target === 'prototype' && canShow(target)) || targets.find(canShow);
}

export async function previewRequest(
  path: string,
  action: 'inspect' | 'start' | 'stop' | 'save' | 'remove' | 'terminal' | 'folder' | 'browser',
  options: { target?: string; install?: boolean; id?: string; config?: PreviewDefinition; requireStackInstructions?: boolean; requireStackCatalog?: boolean } = {},
  signal?: AbortSignal,
): Promise<PreviewTarget[]> {
  const { requireStackInstructions, requireStackCatalog, ...requestOptions } = options;
  const response = await fetch('/api/preview', {
    method: 'POST',
    headers: withServerAuthHeaders({ 'Content-Type': 'application/json' }),
    body: JSON.stringify({ path, action, ...requestOptions }),
    signal,
  });
  if (!response.ok) {
    const message = (await response.text()).trim();
    if (action === 'browser' && message === 'Unknown preview action') throw new Error('Restart Glowbom OSS to open previews in your browser.');
    throw new Error(message || 'Preview request failed.');
  }
  const data = await response.json() as { targets: PreviewTarget[]; stackInstructionsSupported?: boolean; stackCatalogSupported?: boolean };
  if (requireStackCatalog && !data.stackCatalogSupported) throw new Error('Restart Glowbom OSS to enable the stack catalog and saved preview modes.');
  if (requireStackInstructions && !data.stackInstructionsSupported) throw new Error('Restart Glowbom OSS to enable saved stack instructions and build targets.');
  return data.targets;
}

// Split one command into arguments. Shell operators and expansions are not executed.
export function parsePreviewCommand(text: string): string[] {
  const args: string[] = [];
  let current = '';
  let quote = '';
  let escaped = false;
  let started = false;
  for (const char of text.trim()) {
    if (escaped) { current += char; escaped = false; started = true; continue; }
    if (char === '\\' && quote !== "'") { escaped = true; continue; }
    if (quote) {
      if (char === quote) quote = '';
      else current += char;
      continue;
    }
    if (char === '"' || char === "'") { quote = char; started = true; continue; }
    if (/\s/.test(char)) {
      if (started) { args.push(current); current = ''; started = false; }
      continue;
    }
    if ('|&;<>`'.includes(char) || char === '$') throw new Error('Use one command without shell operators or variables. Use {host} and {port} for the local address.');
    current += char;
    started = true;
  }
  if (escaped || quote) throw new Error('Close the quotes and escapes in the launch command.');
  if (started) args.push(current);
  return args;
}

const previewTargetKey = (projectPath: string) => `glowbom.previewTarget:${projectPath}`;

export function readPreviewTarget(projectPath: string): string {
  try { return localStorage.getItem(previewTargetKey(projectPath)) || ''; }
  catch { return ''; }
}

export function writePreviewTarget(projectPath: string, target: string) {
  try { localStorage.setItem(previewTargetKey(projectPath), target); }
  catch { /* The open preview stays selected in this view. */ }
}

// Chat and the full workspace list every inspected stack, including one that is saved but not built yet.
export function listedPreviewTargets(targets: PreviewTarget[]): PreviewTarget[] {
  return targets;
}

// A stack is in the project when its folder exists, or when it was saved as a custom stack.
export function stackIsInProject(target: PreviewTarget): boolean {
  if (target.target.startsWith('custom-')) return true;
  return target.reason !== 'Not built yet' && !target.reason?.startsWith('No ');
}

export function previewStackTitle(target: PreviewTarget): string {
  if (target.target === 'apple' || target.preset === 'swiftui') return 'SwiftUI';
  if (target.target === 'android' || target.preset === 'kotlin') return 'Kotlin + Compose';
  if (target.target === 'prototype') return 'Prototype';
  if (target.target === 'web') return 'Web';
  const name = target.name.trim();
  const label = name || target.target;
  return label.length > 22 ? `${label.slice(0, 20)}…` : label;
}

export function formatPreviewCommand(args: string[]): string {
  return args.map((arg) => /^[\w./{}:=+-]+$/.test(arg) ? arg : JSON.stringify(arg)).join(' ');
}

export interface DiscoveredStack {
  name: string;
  directory: string;
  stack: string;
  preset?: string;
  evidence: string;
  target?: string;
  buildTarget?: string;
  previewMode: 'auto' | 'command' | 'none';
  command?: string[];
  available: boolean;
  needsInstall: boolean;
}

export async function discoverStacks(path: string, signal?: AbortSignal): Promise<{ apps: DiscoveredStack[]; limited: boolean }> {
  const response = await fetch('/api/preview', {
    method: 'POST', headers: withServerAuthHeaders({ 'Content-Type': 'application/json' }),
    body: JSON.stringify({ path, action: 'discover' }), signal,
  });
  if (!response.ok) throw new Error('Could not detect apps. Try again, or restart Glowbom OSS if you just updated it. You can still add a stack manually.');
  return response.json();
}
