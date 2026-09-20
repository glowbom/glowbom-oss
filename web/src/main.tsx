import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import Workspace from './Workspace';
import { applyAppearance, readAppearance } from './lib/appearance';
import './styles.css';

applyAppearance(readAppearance(), window.matchMedia('(prefers-color-scheme: dark)').matches);

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Workspace />
  </StrictMode>,
);
