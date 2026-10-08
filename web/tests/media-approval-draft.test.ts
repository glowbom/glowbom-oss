import { expect, test } from 'bun:test';
import { useMediaApprovalDraft } from '../src/hooks/useMediaApprovalDraft';
import type { OpenCodeMediaApproval } from '../src/types/opencode';
import { hookRunner } from './helpers/react-hook-runner';

test('media edits survive editor remounts and fresh snapshots for the same approval', () => {
  let approval: OpenCodeMediaApproval = { id: 'media-one', title: 'Images', message: '', items: [{ id: 'image-one', mediaType: 'image', prompt: 'Original prompt', provider: 'glowbom' }] };
  const owner = hookRunner(() => useMediaApprovalDraft(approval));
  let draft = owner.render();
  draft.setItems(items => items.map(item => ({ ...item, prompt: 'Personalized travel image', excluded: true, referenceImages: ['data:image/png;base64,reference'] })));
  // The wrapper remains mounted while the inline editor is replaced by details.
  approval = { ...approval, items: approval.items.map(item => ({ ...item })) };
  draft = owner.render();
  expect(draft.items[0]?.prompt).toBe('Personalized travel image');
  expect(draft.items[0]?.excluded).toBe(true);
  expect(draft.items[0]?.referenceImages).toEqual(['data:image/png;base64,reference']);
  const previousEditor = draft.setItems;
  approval = { ...approval, id: 'media-two', items: [{ ...approval.items[0]!, prompt: 'Next request' }] };
  draft = owner.render();
  expect(draft.items[0]?.prompt).toBe('Next request');
  previousEditor(items => items.map(item => ({ ...item, prompt: 'Late old reference' })));
  draft = owner.render();
  expect(draft.items[0]?.prompt).toBe('Next request');
  expect(draft.items[0]?.excluded).toBeUndefined();
  owner.unmount();
});
