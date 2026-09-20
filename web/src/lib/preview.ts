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

export async function previewRequest(
  path: string,
  action: 'inspect' | 'start' | 'stop' | 'save' | 'remove' | 'terminal' | 'folder',
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
  if (!response.ok) throw new Error((await response.text()).trim() || 'Preview request failed.');
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
