import { useEffect, useState } from 'react';
import type { BuildPermissionMode } from '../types/opencode';

export function useBuildPermissionChoice(scope: string) {
  const [choice, setChoice] = useState({ scope, allowAll: false });
  useEffect(() => { setChoice({ scope, allowAll: false }); }, [scope]);
  const allowAll = choice.scope === scope && choice.allowAll;
  return {
    allowAll,
    permissionMode: (allowAll ? 'all' : 'ask') as BuildPermissionMode,
    setAllowAll: (value: boolean) => setChoice({ scope, allowAll: value }),
    onAccepted: () => setChoice({ scope, allowAll: false }),
  };
}
