import { useEffect, useRef } from 'react';
import App from './App';
import { useAppearance } from './components/AppearancePicker';
import { useRefineRun } from './hooks/useRefineRun';
import { didBuildComplete, playBuildCompletionSound } from './lib/build-completion-sound';

export default function Workspace() {
  const appearance = useAppearance();
  const opencode = useRefineRun();
  const cursor = useRefineRun();
  const claude = useRefineRun();
  const codex = useRefineRun();
  const acp = useRefineRun();
  const previous = useRef({ magic: opencode.status, cursor: cursor.status, claude: claude.status, codex: codex.status, acp: acp.status });
  useEffect(() => {
    const next = { magic: opencode.status, cursor: cursor.status, claude: claude.status, codex: codex.status, acp: acp.status };
    if (didBuildComplete(previous.current, next)) playBuildCompletionSound();
    previous.current = next;
  }, [opencode.status, cursor.status, claude.status, codex.status, acp.status]);
  return <App {...appearance} runs={{ opencode, cursor, 'claude-code': claude, codex, acp }} />;
}
