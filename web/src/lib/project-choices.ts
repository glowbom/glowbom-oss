import { canonicalProjectPath, type ChatProject } from './chat';

const hiddenProjectsKey = 'glowbom_hidden_registered_projects';
type ProjectStorage = Pick<Storage, 'getItem' | 'setItem'>;

export function readHiddenRegisteredProjects(storage?: Pick<Storage, 'getItem'>): string[] {
  try {
    const value: unknown = JSON.parse((storage ?? localStorage).getItem(hiddenProjectsKey) || '[]');
    return Array.isArray(value) ? [...new Set(value.filter((path): path is string => typeof path === 'string').map(canonicalProjectPath).filter(Boolean))] : [];
  } catch { return []; }
}

export function hideRegisteredProject(path: string, storage?: ProjectStorage): string[] {
  const source = storage ?? localStorage;
  const hidden = [...new Set([...readHiddenRegisteredProjects(source), canonicalProjectPath(path)].filter(Boolean))];
  source.setItem(hiddenProjectsKey, JSON.stringify(hidden));
  return hidden;
}

// Registered folders extend the picker without becoming recently opened projects.
export function projectChoices(recent: readonly ChatProject[], registered: unknown, hidden: readonly string[] = []): ChatProject[] {
  const result: ChatProject[] = [];
  const seen = new Set<string>();
  for (const project of recent) {
    const path = canonicalProjectPath(project.path);
    if (!path || seen.has(path)) continue;
    seen.add(path);
    result.push({ ...project, path });
  }
  const excluded = new Set(hidden.map(canonicalProjectPath));
  if (!Array.isArray(registered)) return result;
  for (const project of registered) {
    if (!project || project.available !== true || typeof project.path !== 'string' || typeof project.name !== 'string') continue;
    const path = canonicalProjectPath(project.path);
    if (!path || (!path.startsWith('/') && !/^[A-Za-z]:[/\\]/.test(path) && !path.startsWith('\\\\')) || seen.has(path) || excluded.has(path)) continue;
    seen.add(path);
    result.push({ path, name: project.name.trim() || path.split(/[/\\]/).filter(Boolean).at(-1) || path, lastOpenedAt: '' });
  }
  return result;
}
