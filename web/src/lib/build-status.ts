export type BuildStatusSource = 'agent' | 'activity' | 'system';

export interface BuildStatusUpdate {
  text: string;
  source: BuildStatusSource;
  at: string;
}

export interface BuildStatusView {
  current: string;
  recent: string[];
  source: BuildStatusSource;
  at?: string;
}

function concise(text: string): string {
  const parts = text.split(/GLOWBOM_STATUS\s*:/i);
  const latest = parts.reverse().find((part) => part.trim()) || '';
  return latest.replace(/\s+/g, ' ').trim().slice(0, 180);
}

export function parseBuildStatusUpdate(value: unknown): BuildStatusUpdate | null {
  if (!value || typeof value !== 'object') return null;
  const record = value as Record<string, unknown>;
  if (typeof record.text !== 'string') return null;
  const text = concise(record.text);
  if (!text) return null;
  if (record.source !== 'agent' && record.source !== 'activity' && record.source !== 'system') return null;
  const at = typeof record.at === 'string' && Number.isFinite(Date.parse(record.at)) ? record.at : new Date().toISOString();
  return { text, source: record.source, at };
}

function friendlyActivity(line: string): string | null {
  const text = line.trim();
  if (/^Writing the story and drawing the result sketch/i.test(text)) return 'Illustrating and writing the result';
  if (/^Agent is analyzing and refining the project/i.test(text)) return 'Reviewing the project';
  if (/^Starting refinement with OpenCode agent/i.test(text)) return 'Starting the build';
  if (/^🪄 Running media post-pass/i.test(text)) return 'Preparing project media';
  if (/^🧭 Running asset placement reconciliation/i.test(text)) return 'Checking image placement';
  if (/^📋 Implementation Report:/i.test(text)) return 'Summarizing the changes';
  const updated = /^(?:📝 Updated:|📄 Modified:)\s*(.+)$/u.exec(text);
  if (updated) {
    const name = updated[1]?.trim().split(/[\\/]/).at(-1)?.trim();
    return name ? `Updated ${concise(name)}` : null;
  }
  const progress = /^📊 Progress:\s*\d+\/\d+ tasks completed(?:\s*-\s*Working on:\s*(.+))?/u.exec(text);
  if (progress) return progress[1] ? `Working on ${concise(progress[1])}` : 'Working through the build steps';
  const tool = /^🔧 Running:\s*([\w.-]+)/u.exec(text);
  if (tool) {
    const name = tool[1]?.toLowerCase();
    if (name === 'bash' || name === 'shell' || name === 'exec_command') return 'Running a project command';
    if (name === 'read' || name === 'glob' || name === 'grep') return 'Reviewing project files';
    if (name === 'edit' || name === 'write' || name === 'apply_patch') return 'Updating project files';
    return 'Working in the project';
  }
  return null;
}

function uniqueRecent(values: string[]): string[] {
  const result: string[] = [];
  for (const value of values) if (value && !result.includes(value)) result.push(value);
  return result;
}

export function buildStatusView(input: {
  updates: BuildStatusUpdate[];
  logs: string[];
  awaitingInput?: boolean;
  awaitingPermission?: boolean;
  awaitingMedia?: boolean;
}): BuildStatusView {
  const updates = input.updates.map((update) => ({ ...update, text: concise(update.text) })).filter((update) => update.text);
  const reported = uniqueRecent(updates.map((update) => update.text));
  const recorded = uniqueRecent(input.logs.map(friendlyActivity).filter((line): line is string => !!line));
  const latest = updates.at(-1);
  const waiting = input.awaitingMedia ? 'Waiting for your media choice'
    : input.awaitingPermission ? 'Waiting for your permission'
      : input.awaitingInput ? 'Waiting for your answer' : '';
  const current = waiting || latest?.text || recorded.at(-1) || 'Starting the build';
  const recent = uniqueRecent([
    ...reported.filter((line) => line !== current).reverse(),
    ...recorded.filter((line) => line !== current).reverse(),
  ]).slice(0, 3);
  return { current, recent, source: waiting ? 'system' : latest?.source || 'activity', at: latest?.at };
}
