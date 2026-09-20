import { useState } from 'react';
import App from './App';
import { WelcomeScreen } from './components/WelcomeScreen';
import { useAppearance } from './components/AppearancePicker';

const welcomeKey = 'glowbom_oss_welcome_complete';
function hasContinued() {
  try { return localStorage.getItem(welcomeKey) === 'true'; } catch { return false; }
}

export default function Workspace() {
  const appearance = useAppearance();
  const [entered, setEntered] = useState(hasContinued);
  const [showWelcome, setShowWelcome] = useState(!entered);
  const continueToWorkspace = () => {
    try { localStorage.setItem(welcomeKey, 'true'); } catch { /* Continue when storage is unavailable. */ }
    setEntered(true);
    setShowWelcome(false);
  };
  return <>
    {showWelcome && <WelcomeScreen {...appearance} onContinue={continueToWorkspace} />}
    {entered && <div hidden={showWelcome}><App {...appearance} onOpenAccount={() => setShowWelcome(true)} /></div>}
  </>;
}
