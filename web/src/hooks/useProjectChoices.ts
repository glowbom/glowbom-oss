import { useCallback, useEffect, useState } from 'react';
import type { ChatProject } from '../lib/chat';
import { hideRegisteredProject, projectChoices, readHiddenRegisteredProjects } from '../lib/project-choices';
import { loadStudioProjects, type StudioProject } from '../lib/studio';

export function useProjectChoices(recent: readonly ChatProject[], open: boolean) {
  const [registered, setRegistered] = useState<StudioProject[]>([]);
  const [hidden, setHidden] = useState(readHiddenRegisteredProjects);
  useEffect(() => {
    if (!open) return;
    let request: AbortController | null = null;
    const refresh = () => {
      if (document.visibilityState === 'hidden' || request) return;
      setHidden(readHiddenRegisteredProjects());
      const controller = new AbortController();
      request = controller;
      void loadStudioProjects(controller.signal, { metadataOnly: true }).then(result => {
        if (!controller.signal.aborted && result.supported) setRegistered(result.projects);
      }).catch(() => { /* Keep recent and previously discovered projects available. */ })
        .finally(() => { if (request === controller) request = null; });
    };
    refresh();
    window.addEventListener('focus', refresh);
    document.addEventListener('visibilitychange', refresh);
    return () => {
      request?.abort();
      window.removeEventListener('focus', refresh);
      document.removeEventListener('visibilitychange', refresh);
    };
  }, [open]);
  const hide = useCallback((path: string) => setHidden(hideRegisteredProject(path)), []);
  return { choices: projectChoices(recent, registered, hidden), hide };
}
