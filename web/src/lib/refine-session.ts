export function canContinueRefineSession(previous: { projectPath: string; driver: string; model: string }, next: { projectPath: string; driver: string; model: string }, enabled: boolean): boolean {
  return enabled && previous.projectPath === next.projectPath && previous.driver === next.driver
    && (next.driver !== 'acp' || previous.model === next.model);
}
