import { allBuildPermissionScope, cursorPermissionScope } from '../lib/agent-permissions';

export function BuildPermissionControl({ driver, checked, disabled = false, onChange }: {
  driver: string; checked: boolean; disabled?: boolean; onChange(value: boolean): void;
}) {
  if (driver === 'cursor') return <p className="work-note">{cursorPermissionScope}</p>;
  return <div className="work-permission-control">
    <label><input type="checkbox" checked={checked} disabled={disabled} onChange={event => onChange(event.target.checked)} /> Allow all for this build</label>
    <p className="work-note">{allBuildPermissionScope}</p>
  </div>;
}
