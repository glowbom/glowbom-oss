import { afterEach, expect, test } from 'bun:test';
import { renderToStaticMarkup } from 'react-dom/server';
import { MediaApprovalEditor } from '../src/components/MediaApprovalEditor';
import type { OpenCodeMediaApproval } from '../src/types/opencode';
import { rememberImageSessionKey } from '../src/lib/project-icon';

const originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
afterEach(() => {
  for (const source of ['openai-api', 'gemini-api', 'xai-api']) rememberImageSessionKey(source, '');
  if (originalStorage) Object.defineProperty(globalThis, 'localStorage', originalStorage);
  else Reflect.deleteProperty(globalThis, 'localStorage');
});

const approval: OpenCodeMediaApproval = { id: 'review-1', title: 'Review assets', message: 'Choose what to create.', items: [
  { id: 'image-1', mediaType: 'image', prompt: 'A garden scene', provider: 'OpenAI', sourceId: 'openai-api', aspectRatio: '4:3', referenceImages: ['data:image/png;base64,cGhvdG8='] },
  { id: 'video-1', mediaType: 'video', prompt: 'Slow pan', provider: 'Veo', sourceId: 'veo-api', fromKey: 'image-1', aspectRatio: '16:9' },
  { id: 'audio-1', mediaType: 'audio', prompt: 'Welcome home', provider: 'ElevenLabs', sourceId: 'elevenlabs-api', audioType: 'voice', voiceID: 'voice-123', modelID: 'model-123', usagePrompt: 'Read this greeting when the app opens' },
] };

function approvalRows(html: string): string[] {
  return html.match(/<article\b[\s\S]*?<\/article>/g) || [];
}

function controlledPanel(row: string, assetNumber: number): { button: string; panel: string; summary: string } {
  const button = (row.match(/<button\b[^>]*>/g) || []).find((tag) => tag.includes(`aria-label="Edit asset ${assetNumber}"`)) || '';
  const panelID = button.match(/aria-controls="([^"]+)"/)?.[1];
  const panel = (row.match(/<(?:div|fieldset)\b[^>]*>/g) || []).find((tag) => panelID && tag.includes(`id="${panelID}"`)) || '';
  return { button, panel, summary: panel ? row.slice(0, row.indexOf(panel)) : '' };
}

test('asset rows begin collapsed with accessible controls and mounted hidden settings', () => {
  const html = renderToStaticMarkup(<MediaApprovalEditor approval={approval} busy={false} onRespond={async () => true} />);
  const rows = approvalRows(html);
  expect(rows).toHaveLength(3);
  const panelIDs: string[] = [];
  rows.forEach((row, index) => {
    const { button, panel } = controlledPanel(row, index + 1);
    expect(button).toContain('aria-expanded="false"');
    expect(button).toContain('aria-controls="');
    expect(panel).toContain('hidden=""');
    expect(row).toContain(index === 0 ? 'Provider' : 'Generate with');
    expect(row).toContain('<textarea');
    expect(row).toContain('How the app should use it');
    panelIDs.push(button.match(/aria-controls="([^"]+)"/)?.[1] || '');
  });
  expect(new Set(panelIDs).size).toBe(rows.length);
});

test('row disclosures and inclusion controls describe their visible prompt and metadata', () => {
  const rows = approvalRows(renderToStaticMarkup(<MediaApprovalEditor approval={approval} busy={false} onRespond={async () => true} />));
  rows.forEach((row, index) => {
    const { button, summary } = controlledPanel(row, index + 1);
    const descriptionIDs = (button.match(/aria-describedby="([^"]+)"/)?.[1] || '').split(' ').filter(Boolean);
    expect(descriptionIDs).toHaveLength(2);
    descriptionIDs.forEach((id) => expect(summary).toContain(`id="${id}"`));
    const includeControl = (row.match(/<input\b[^>]*>/g) || []).find((tag) => tag.includes(`aria-label="Include asset ${index + 1}"`)) || '';
    expect(includeControl).toContain(`aria-describedby="${descriptionIDs[0]}"`);
    expect(summary).toContain(approval.items[index]!.prompt);
  });
});

test('disclosure panel identities follow assets when row positions change', () => {
  const originalRows = approvalRows(renderToStaticMarkup(<MediaApprovalEditor approval={approval} busy={false} onRespond={async () => true} />));
  const reordered = { ...approval, items: [...approval.items].reverse() };
  const reorderedRows = approvalRows(renderToStaticMarkup(<MediaApprovalEditor approval={reordered} busy={false} onRespond={async () => true} />));
  originalRows.forEach((row, index) => {
    const originalID = controlledPanel(row, index + 1).button.match(/aria-controls="([^"]+)"/)?.[1];
    const newPosition = originalRows.length - index;
    const reorderedID = controlledPanel(reorderedRows[newPosition - 1] || '', newPosition).button.match(/aria-controls="([^"]+)"/)?.[1];
    expect(originalID).toBeTruthy();
    expect(reorderedID).toBe(originalID);
  });
});

test('closed rows show prompts and friendly source, shape, and reference summaries', () => {
  const html = renderToStaticMarkup(<MediaApprovalEditor approval={approval} busy={false} onRespond={async () => true} />);
  const rows = approvalRows(html);
  const imageSummary = controlledPanel(rows[0] || '', 1).summary;
  const videoSummary = controlledPanel(rows[1] || '', 2).summary;
  const audioSummary = controlledPanel(rows[2] || '', 3).summary;
  expect(imageSummary).toContain('A garden scene');
  expect(imageSummary).toContain('OpenAI API');
  expect(imageSummary).toContain('4:3');
  expect(imageSummary).toContain('1 reference');
  expect(imageSummary).not.toContain('openai-api');
  expect(videoSummary).toContain('Slow pan');
  expect(videoSummary).toContain('Google API · Veo 3.1 Lite');
  expect(videoSummary).toContain('16:9');
  expect(audioSummary).toContain('Welcome home');
  expect(audioSummary).toContain('ElevenLabs API');
  const subscription = { ...approval, items: [{ ...approval.items[0]!, sourceId: 'openai-subscription' }] };
  const subscriptionRow = approvalRows(renderToStaticMarkup(<MediaApprovalEditor approval={subscription} busy={false} onRespond={async () => true} />))[0] || '';
  expect(controlledPanel(subscriptionRow, 1).summary).toContain('ChatGPT subscription');
});

test('source summaries follow the edited source instead of the original provider label', () => {
  const switched = { ...approval, items: [{ ...approval.items[0]!, provider: 'OpenAI', sourceId: 'gemini-api' }] };
  const row = approvalRows(renderToStaticMarkup(<MediaApprovalEditor approval={switched} busy={false} onRespond={async () => true} />))[0] || '';
  const summary = controlledPanel(row, 1).summary;
  expect(summary).toContain('Google API · Gemini 3.1 Flash Lite Image');
  expect(summary).not.toContain('OpenAI');
  expect(summary).not.toContain('gemini-api');
});

test('previous-image references are named in the closed row and explained beside their preview', () => {
  const previous: OpenCodeMediaApproval = { ...approval, items: [{ ...approval.items[0]!, referenceOrigin: 'previous-image' }] };
  const html = renderToStaticMarkup(<MediaApprovalEditor approval={previous} busy={false} onRespond={async () => true} />);
  const row = approvalRows(html)[0] || '';
  const { summary, panel } = controlledPanel(row, 1);
  expect(summary).toContain('Previous image');
  expect(summary).not.toContain('1 reference');
  expect(panel).toContain('hidden=""');
  expect(row).toContain('Using the previous image to guide this version. Replace it or remove it to start fresh.');
  expect(row).toContain('<figcaption class="media-editor-note">Previous image attached</figcaption>');
  expect(row).toContain('alt="Reference for asset 1"');
  expect(row).toContain('Replace reference');
  expect(row).toContain('Remove reference');
});

test('fresh image rows visibly say No reference even if empty provenance was retained in old data', () => {
  for (const referenceOrigin of [undefined, 'previous-image'] as const) {
    const fresh: OpenCodeMediaApproval = { ...approval, items: [{ ...approval.items[0]!, referenceImages: [], referenceOrigin }] };
    const html = renderToStaticMarkup(<MediaApprovalEditor approval={fresh} busy={false} onRespond={async () => true} />);
    const row = approvalRows(html)[0] || '';
    expect(controlledPanel(row, 1).summary).toContain('No reference');
    expect(row).not.toContain('Previous image');
    expect(row).not.toContain('<img');
    expect(row).toContain('Add reference');
    expect(row).not.toContain('Remove reference');
  }
});

test('blank added rows have readable titles while usage instructions remain required', () => {
  const blank: OpenCodeMediaApproval = { ...approval, items: ['image', 'video', 'audio'].map((mediaType) => ({ id: `added-${mediaType}`, mediaType, prompt: '', provider: '', usagePrompt: '' })) };
  const rows = approvalRows(renderToStaticMarkup(<MediaApprovalEditor approval={blank} busy={false} onRespond={async () => true} />));
  rows.forEach((row, index) => {
    expect(controlledPanel(row, index + 1).summary).toContain(`New ${blank.items[index]!.mediaType}`);
    expect(row).toContain('Required for added assets');
  });
});

test('approval editor keeps connection controls shared outside per-asset rows', () => {
  const html = renderToStaticMarkup(<MediaApprovalEditor approval={approval} busy={false} onRespond={async () => true} />);
  expect(html).toContain('3 of 3 assets selected');
  expect(html.match(/aria-label="Include asset \d+"/g)).toHaveLength(3);
  expect(html.match(/Generation prompt/g)).toHaveLength(2);
  expect(html).toContain('Words to speak');
  expect(html).toContain('Reference image');
  expect(html).toContain('Remove reference');
  expect(html).toContain('alt="Reference for asset 1"');
  expect(html).toContain('Starting image');
  expect(html).toContain('Voice ID');
  expect(html).toContain('value="voice-123"');
  expect(html).toContain('value="model-123"');
  expect(html).toContain('value="A garden scene"');
  expect(html).toContain('Read this greeting when the app opens');
  expect(html).toContain('Add asset');
  expect(html).toContain('How the app should use it');
  expect(html).toContain('Skip all');
  expect(html).toContain('<summary>Media connections</summary>');
  expect(html.match(/type="password"/g)).toHaveLength(2);
  expect(html).toContain('shared by icons and Build');
  for (const article of approvalRows(html)) expect(article).not.toContain('type="password"');
  expect(html.indexOf('<summary>Media connections</summary>')).toBeGreaterThan(html.lastIndexOf('</article>'));
});

test('shared connections show session entries and never copy saved provider secrets into password fields', () => {
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: { getItem: (key: string) => key === 'glowbom_oss_image_provider_keys' ? JSON.stringify({ openaiImageKey: 'saved-openai-secret', geminiImageKey: 'saved-gemini-secret' }) : null } });
  const savedHTML = renderToStaticMarkup(<MediaApprovalEditor approval={approval} busy={false} onRespond={async () => true} />);
  expect(savedHTML).not.toContain('saved-openai-secret');
  expect(savedHTML).not.toContain('saved-gemini-secret');
  expect(savedHTML.match(/type="password"[^>]*value=""/g)).toHaveLength(2);
  rememberImageSessionKey('gemini-api', 'entered-session-key');
  const sessionHTML = renderToStaticMarkup(<MediaApprovalEditor approval={approval} busy={false} onRespond={async () => true} />);
  expect(sessionHTML).toContain('value="entered-session-key"');
  expect(sessionHTML).not.toContain('saved-openai-secret');
});

test('repeated selected providers share a single key field and excluded providers do not appear', () => {
  const sameProvider: OpenCodeMediaApproval = { ...approval, items: [approval.items[0]!, { ...approval.items[0]!, id: 'image-2' }, { ...approval.items[0]!, id: 'xai-excluded', sourceId: 'xai-api', excluded: true }] };
  const html = renderToStaticMarkup(<MediaApprovalEditor approval={sameProvider} busy={false} onRespond={async () => true} />);
  expect(html.match(/type="password"/g)).toHaveLength(1);
  expect(html).toContain('OpenAI API key');
  expect(html).not.toContain('xAI API key');
});

test('excluded assets stay visible and the all-excluded decision offers Skip all', () => {
  const excluded = { ...approval, items: approval.items.map((item) => ({ ...item, excluded: true })) };
  const html = renderToStaticMarkup(<MediaApprovalEditor approval={excluded} busy={false} onRespond={async () => true} />);
  expect(html).toContain('0 of 3 assets selected');
  expect(html.match(/media-editor-excluded/g)).toHaveLength(3);
  for (const [index, row] of approvalRows(html).entries()) {
    const includeControl = (row.match(/<input\b[^>]*>/g) || []).find((tag) => tag.includes(`aria-label="Include asset ${index + 1}"`)) || '';
    expect(includeControl).toContain('type="checkbox"');
    expect(includeControl).not.toContain('checked=""');
    expect(controlledPanel(row, index + 1).summary).toContain('Excluded');
  }
  expect(html).toContain('disabled="">Create selected (0)');
  expect(html).toContain('>Skip all</button>');
  expect(html).toContain('A garden scene');
});

test('a selected video marks its excluded starting frame as needing attention while collapsed', () => {
  const dependent: OpenCodeMediaApproval = { ...approval, items: [
    { ...approval.items[0]!, excluded: true },
    { ...approval.items[1]!, fromKey: approval.items[0]!.prompt },
  ] };
  const html = renderToStaticMarkup(<MediaApprovalEditor approval={dependent} busy={false} onRespond={async () => true} />);
  const videoRow = approvalRows(html)[1] || '';
  const { button, panel, summary } = controlledPanel(videoRow, 2);
  expect(button).toContain('aria-expanded="false"');
  expect(panel).toContain('hidden=""');
  expect(summary).toContain('Needs attention');
  const settings = videoRow.slice(videoRow.indexOf(panel));
  expect(settings).toContain('class="media-editor-error"');
  expect(settings).toContain('Include the video’s starting image or choose another starting frame.');
  expect(html).toContain('disabled="">Create selected (1)');
});

test('busy approval locks all editing and decisions while errors preserve the visible draft', () => {
  const html = renderToStaticMarkup(<MediaApprovalEditor approval={approval} busy error="The selected source could not connect. Refresh sources." onRespond={async () => false} />);
  expect(html.match(/<fieldset disabled=""/g)).toHaveLength(3);
  expect(html).toContain('disabled="">Sending…');
  expect(html).toContain('disabled="">Skip all');
  expect(html).toContain('role="alert">The selected source could not connect. Refresh sources.');
  expect(html).toContain('A garden scene');
  expect(html).toContain('value="voice-123"');
  for (const [index, row] of approvalRows(html).entries()) {
    const includeControl = (row.match(/<input\b[^>]*>/g) || []).find((tag) => tag.includes(`aria-label="Include asset ${index + 1}"`)) || '';
    expect(includeControl).toContain('disabled=""');
  }
});

test('sound and music render their own generation parameters', () => {
  const sounds: OpenCodeMediaApproval = { ...approval, items: [
    { id: 'sound-1', mediaType: 'audio', prompt: 'Chime', provider: 'ElevenLabs', sourceId: 'elevenlabs-api', audioType: 'sound', promptInfluence: 0.4, loop: true },
    { id: 'added-music', mediaType: 'audio', prompt: 'Soft piano', provider: 'ElevenLabs', sourceId: 'elevenlabs-api', audioType: 'music', durationSeconds: 60, forceInstrumental: true },
  ] };
  const html = renderToStaticMarkup(<MediaApprovalEditor approval={sounds} busy={false} onRespond={async () => true} />);
  expect(html).toContain('Prompt influence');
  expect(html).toContain('value="0.4"');
  expect(html).toContain('Loop this sound');
  expect(html).toContain('Instrumental only');
  expect(html).toContain('max="600"');
  expect(html).toContain('value="60"');
  expect(controlledPanel(approvalRows(html)[1] || '', 2).summary).toContain('60 seconds');
  expect(controlledPanel(approvalRows(html)[1] || '', 2).summary).toContain('Instrumental');
  expect(html).toContain('Required for added assets');
  expect(html).toContain('Remove added asset');
  expect(html).not.toContain('Voice ID');
  expect(html).toContain('Describe how the app should use this added asset.');
  for (const [index, row] of approvalRows(html).entries()) expect(controlledPanel(row, index + 1).panel).toContain('hidden=""');
});

test('music reviews start at an explicit 30 seconds with no automatic music length', () => {
  const music: OpenCodeMediaApproval = { ...approval, items: [{ id: 'music-1', mediaType: 'audio', prompt: 'Soft piano', provider: 'ElevenLabs', sourceId: 'elevenlabs-api', audioType: 'music' }] };
  const html = renderToStaticMarkup(<MediaApprovalEditor approval={music} busy={false} onRespond={async () => true} />);
  const { summary } = controlledPanel(approvalRows(html)[0] || '', 1);
  expect(summary).toContain('30 seconds');
  expect(html).toContain('value="30"');
  expect(html).toContain('required=""');
  expect(html).not.toContain('placeholder="Automatic"');
  expect(html).toContain('longer tracks can use more credits');
});
