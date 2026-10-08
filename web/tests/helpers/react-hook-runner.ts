import React from 'react';

// Run hook state and effects without a browser or replacing the React module.
// Keep this adapter isolated from production code and restore the dispatcher.
export function hookRunner<T>(renderHook: () => T) {
  const internals = (React as unknown as {
    __CLIENT_INTERNALS_DO_NOT_USE_OR_WARN_USERS_THEY_CANNOT_UPGRADE: { H: unknown };
  }).__CLIENT_INTERNALS_DO_NOT_USE_OR_WARN_USERS_THEY_CANNOT_UPGRADE;
  const slots: { value?: any; dependencies?: readonly unknown[]; cleanup?: () => void }[] = [];
  let index = 0;
  let unmounted = false;
  let lateWrites = 0;
  const pendingEffects: (() => void)[] = [];
  const changed = (left?: readonly unknown[], right?: readonly unknown[]) => !left || !right || left.length !== right.length || left.some((value, offset) => !Object.is(value, right[offset]));
  const dispatcher = {
    useState(initial: any) {
      const position = index++;
      const slot = slots[position] ||= { value: typeof initial === 'function' ? initial() : initial };
      return [slot.value, (next: any) => {
        if (unmounted) lateWrites++;
        slot.value = typeof next === 'function' ? next(slot.value) : next;
      }];
    },
    useRef(initial: any) { return (slots[index++] ||= { value: { current: initial } }).value; },
    useId() { const position = index++; return (slots[position] ||= { value: `test-id-${position}` }).value; },
    useMemo(factory: () => any, dependencies?: readonly unknown[]) {
      const slot = slots[index++] ||= {};
      if (changed(slot.dependencies, dependencies)) { slot.value = factory(); slot.dependencies = dependencies; }
      return slot.value;
    },
    useCallback(callback: (...args: any[]) => any, dependencies?: readonly unknown[]) { return dispatcher.useMemo(() => callback, dependencies); },
    useEffect(effect: () => (() => void) | void, dependencies?: readonly unknown[]) {
      const slot = slots[index++] ||= {};
      if (changed(slot.dependencies, dependencies)) {
        slot.dependencies = dependencies;
        pendingEffects.push(() => { slot.cleanup?.(); slot.cleanup = effect() || undefined; });
      }
    },
    useLayoutEffect(effect: () => (() => void) | void, dependencies?: readonly unknown[]) {
      dispatcher.useEffect(effect, dependencies);
    },
  };
  return {
    render(): T {
      index = 0;
      const previous = internals.H;
      internals.H = dispatcher;
      let result: T;
      try { result = renderHook(); }
      finally { internals.H = previous; }
      for (const effect of pendingEffects.splice(0)) effect();
      return result;
    },
    unmount() { for (const slot of slots) slot.cleanup?.(); unmounted = true; },
    get lateWrites() { return lateWrites; },
  };
}
