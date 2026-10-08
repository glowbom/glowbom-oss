import { expect, test } from 'bun:test';
import { readBuildCompletionSound, readUseJev, saveBuildCompletionSound, saveUseJev, useJevForBuild } from '../src/lib/build-settings';
test('Jev defaults off, persists, and is restricted to OpenCode builds', () => {
 const previous = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
 const values = new Map<string,string>();
 Object.defineProperty(globalThis,'localStorage',{ configurable:true,value:{getItem:(key:string)=>values.get(key)??null,setItem:(key:string,value:string)=>values.set(key,value)}});
 try {
  expect(readUseJev()).toBe(false); saveUseJev(true);
  expect(readUseJev()).toBe(true); expect(useJevForBuild()).toBe(true); expect(useJevForBuild('opencode')).toBe(true); expect(useJevForBuild('cursor')).toBe(false); expect(useJevForBuild('codex')).toBe(false);
  saveUseJev(false); expect(useJevForBuild('opencode')).toBe(false);
 } finally { if(previous)Object.defineProperty(globalThis,'localStorage',previous);else Reflect.deleteProperty(globalThis,'localStorage'); }
});

test('build completion sound defaults on and can be disabled', () => {
 const previous = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
 const values = new Map<string,string>();
 Object.defineProperty(globalThis,'localStorage',{ configurable:true,value:{getItem:(key:string)=>values.get(key)??null,setItem:(key:string,value:string)=>values.set(key,value)}});
 try {
  expect(readBuildCompletionSound()).toBe(true);
  saveBuildCompletionSound(false); expect(readBuildCompletionSound()).toBe(false);
  saveBuildCompletionSound(true); expect(readBuildCompletionSound()).toBe(true);
 } finally { if(previous)Object.defineProperty(globalThis,'localStorage',previous);else Reflect.deleteProperty(globalThis,'localStorage'); }
});
