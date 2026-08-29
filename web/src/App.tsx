import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  MODEL_PROVIDERS,
  decodeModelOptionValue,
  encodeModelOptionValue,
  modelCatalogGroups,
  providerDefinition,
  resolveRefineModel,
} from './lib/model-catalog';
import { lineLooksLikeMarkdown, parseConsoleHeading, parseConsoleMarkdown } from './lib/console-render';
import { ApiError, openCodeApi, toErrorMessage } from './lib/api';
import {
  OPENCODE_DEFAULT_MODEL_VALUE,
  OPENCODE_RECOMMENDED_MODEL_VALUE,
  findChatGPTRecommendedModel,
  resolveOpenCodeModelValue,
} from './lib/opencode-model-recommendation';
import { useRefineRun } from './hooks/useRefineRun';
import type {
  AuthProviderID,
  ImageProviderID,
  ImageProviderKeyState,
  OpenCodeAvailableProvider,
  OpenAIAuthMode,
  OpenAIModelsResponseModel,
  OpenCodeIDE,
  OpenCodeAuthStatus,
  OpenCodeHistoryStatus,
  OpenCodeInstructionPickedFile,
  OpenCodeHealthResponse,
  OpenCodeProjectHistoryAttachment,
  OpenCodeProjectHistoryEntry,
  OpenCodeProjectIDEAction,
  OpenCodeProjectIDEStatusResponse,
  OpenCodeProject,
  OpenCodeProjectEnvelope,
  OpenCodeQuestionItem,
  ProviderID,
  ProviderKeyState,
} from './types/opencode';

type CredentialMode = 'auth' | 'api-key' | 'opencode-config';
const MAX_INSTRUCTION_ATTACHMENT_BYTES = 40 * 1024 * 1024;
const PROJECT_HISTORY_STORAGE_KEY = 'glowbom_oss_project_history';
const LEGACY_PROJECT_HISTORY_STORAGE_KEY = 'glowby_oss_project_history';
const DEFAULT_BUILD_INSTRUCTIONS = 'Make this project production ready. Follow AGENTS.md when present.';

const RUN_STATUS_LABEL: Record<string, string> = {
  idle: 'Ready',
  running: 'Building',
  completed: 'Done',
  failed: 'Needs attention',
  cancelled: 'Stopped',
};

const DEFAULT_PROVIDER_KEYS: ProviderKeyState = {
  openaiKey: '',
  anthropicKey: '',
  geminiKey: '',
  fireworksKey: '',
  openrouterKey: '',
  opencodeZenKey: '',
  xaiKey: '',
  elevenLabsKey: '',
};

const PROVIDER_KEYS_STORAGE_KEY = 'glowbom_oss_provider_keys';
const LEGACY_PROVIDER_KEYS_STORAGE_KEY = 'glowby_oss_provider_keys';

function readMigratedLocalStorage(key: string, legacyKey: string): string | null {
  const currentValue = localStorage.getItem(key);
  if (currentValue !== null) {
    return currentValue;
  }

  const legacyValue = localStorage.getItem(legacyKey);
  if (legacyValue !== null) {
    localStorage.setItem(key, legacyValue);
  }
  return legacyValue;
}

function loadProviderKeys(): ProviderKeyState {
  try {
    const raw = readMigratedLocalStorage(PROVIDER_KEYS_STORAGE_KEY, LEGACY_PROVIDER_KEYS_STORAGE_KEY);
    if (raw) {
      const parsed = JSON.parse(raw);
      return { ...DEFAULT_PROVIDER_KEYS, ...parsed };
    }
  } catch {
    // ignore corrupt data
  }
  return { ...DEFAULT_PROVIDER_KEYS };
}

function saveProviderKeys(keys: ProviderKeyState): void {
  try {
    localStorage.setItem(PROVIDER_KEYS_STORAGE_KEY, JSON.stringify(keys));
  } catch {
    // ignore storage errors
  }
}

const DEFAULT_IMAGE_PROVIDER_KEYS: ImageProviderKeyState = {
  openaiImageKey: '',
  geminiImageKey: '',
  xaiImageKey: '',
};

const IMAGE_PROVIDER_KEYS_STORAGE_KEY = 'glowbom_oss_image_provider_keys';

function loadImageProviderKeys(): ImageProviderKeyState {
  try {
    const raw = localStorage.getItem(IMAGE_PROVIDER_KEYS_STORAGE_KEY);
    if (raw) {
      const parsed = JSON.parse(raw);
      return { ...DEFAULT_IMAGE_PROVIDER_KEYS, ...parsed };
    }

    const providerKeys = loadProviderKeys();
    const migratedKeys = {
      openaiImageKey: providerKeys.openaiKey,
      geminiImageKey: providerKeys.geminiKey,
      xaiImageKey: providerKeys.xaiKey,
    };
    if (Object.values(migratedKeys).some((value) => value.trim())) {
      saveImageProviderKeys(migratedKeys);
    }
    return migratedKeys;
  } catch {
    return { ...DEFAULT_IMAGE_PROVIDER_KEYS };
  }
}

function saveImageProviderKeys(keys: ImageProviderKeyState): void {
  try {
    localStorage.setItem(IMAGE_PROVIDER_KEYS_STORAGE_KEY, JSON.stringify(keys));
  } catch {
    // ignore storage errors
  }
}

const IMAGE_SOURCE_STORAGE_KEY = 'glowbom_oss_image_source';
const LEGACY_IMAGE_SOURCE_STORAGE_KEY = 'glowby_oss_image_source';
const OPENAI_IMAGE_SOURCE = 'Glowbom Images (gpt-image-2)';
const GEMINI_IMAGE_SOURCE = 'Glowbom Images (Nano Banana 2)';
const XAI_IMAGE_SOURCE = 'Glowbom Images (Grok Imagine Image Quality)';

const IMAGE_PROVIDERS: Array<{
  id: ImageProviderID;
  label: string;
  source: string;
  keyField: keyof ImageProviderKeyState;
  keyLabel: string;
  keyPlaceholder: string;
}> = [
  {
    id: 'openai',
    label: 'GPT Image 2',
    source: OPENAI_IMAGE_SOURCE,
    keyField: 'openaiImageKey',
    keyLabel: 'OpenAI image key',
    keyPlaceholder: 'sk-...',
  },
  {
    id: 'gemini',
    label: 'Nano Banana 2',
    source: GEMINI_IMAGE_SOURCE,
    keyField: 'geminiImageKey',
    keyLabel: 'Gemini image key',
    keyPlaceholder: 'Gemini key',
  },
  {
    id: 'xai',
    label: 'Grok Imagine Image Quality',
    source: XAI_IMAGE_SOURCE,
    keyField: 'xaiImageKey',
    keyLabel: 'xAI image key',
    keyPlaceholder: 'xAI key',
  },
];
const DEFAULT_IMAGE_PROVIDER = IMAGE_PROVIDERS[0]!;

function loadImageSource(): string {
  try {
    return readMigratedLocalStorage(IMAGE_SOURCE_STORAGE_KEY, LEGACY_IMAGE_SOURCE_STORAGE_KEY) || '';
  } catch {
    return '';
  }
}

function saveImageSource(value: string): void {
  try {
    localStorage.setItem(IMAGE_SOURCE_STORAGE_KEY, value);
  } catch {
    // ignore storage errors
  }
}

const CREDENTIAL_MODE_STORAGE_KEY = 'glowbom_oss_credential_mode';
const LEGACY_CREDENTIAL_MODE_STORAGE_KEY = 'glowby_oss_credential_mode';
const VALID_CREDENTIAL_MODES: CredentialMode[] = ['opencode-config'];

function loadCredentialMode(): CredentialMode {
  try {
    const raw = readMigratedLocalStorage(CREDENTIAL_MODE_STORAGE_KEY, LEGACY_CREDENTIAL_MODE_STORAGE_KEY);
    if (raw && VALID_CREDENTIAL_MODES.includes(raw as CredentialMode)) {
      return raw as CredentialMode;
    }
  } catch {
    // ignore
  }
  return 'opencode-config';
}

function saveCredentialMode(mode: CredentialMode): void {
  try {
    localStorage.setItem(CREDENTIAL_MODE_STORAGE_KEY, mode);
  } catch {
    // ignore
  }
}

const TARGET_SELECTION_STORAGE_KEY = 'glowbom_oss_selected_targets';
const LEGACY_TARGET_SELECTION_STORAGE_KEY = 'glowby_oss_selected_targets';

const BUILD_TARGETS = [
  { id: 'prototype', label: 'Prototype', dir: 'prototype' },
  { id: 'apple', label: 'Apple', dir: 'apple' },
  { id: 'android', label: 'Android', dir: 'android' },
  { id: 'web', label: 'Web', dir: 'web' },
] as const;

const ALL_TARGET_IDS: string[] = BUILD_TARGETS.map((t) => t.id);

function loadSelectedTargets(): string[] {
  try {
    const raw = readMigratedLocalStorage(TARGET_SELECTION_STORAGE_KEY, LEGACY_TARGET_SELECTION_STORAGE_KEY);
    if (raw) {
      const parsed = JSON.parse(raw);
      if (Array.isArray(parsed)) {
        const valid = parsed.filter((id: string) => ALL_TARGET_IDS.includes(id));
        return valid.length > 0 ? valid : [...ALL_TARGET_IDS];
      }
    }
  } catch {
    // ignore
  }
  return [...ALL_TARGET_IDS];
}

function saveSelectedTargets(ids: string[]): void {
  try {
    localStorage.setItem(TARGET_SELECTION_STORAGE_KEY, JSON.stringify(ids));
  } catch {
    // ignore
  }
}

function normalizeImageSource(source: string): string {
  if (
    source === 'Glowbom Images' ||
    source === 'Glowby Images' ||
    source === 'Glowby Images (gpt-image-1)' ||
    source === 'Glowby Images (gpt-image-1.5)'
  ) {
    return OPENAI_IMAGE_SOURCE;
  }
  if (
    source === 'Glowby Images (Grok Imagine Image Pro)' ||
    source === 'Grok Imagine Image Pro' ||
    source === 'Glowby Images (Grok 2 Image Gen)' ||
    source === 'Grok 2 Image Gen'
  ) {
    return XAI_IMAGE_SOURCE;
  }
  if (source.startsWith('Glowby Images')) {
    return source.replace(/^Glowby Images/, 'Glowbom Images');
  }
  return source;
}

function validateImageSource(source: string): string {
  const normalizedSource = normalizeImageSource(source);
  return IMAGE_PROVIDERS.some((provider) => provider.source === normalizedSource)
    ? normalizedSource
    : OPENAI_IMAGE_SOURCE;
}

function suggestBundleID(name: string): string {
  const slug = name
    .toLowerCase()
    .replace(/[^a-z0-9]/g, '.')
    .replace(/\.{2,}/g, '.')
    .replace(/^\.+|\.+$/g, '')
    .split('.')
    .filter(Boolean)
    .map((part) => (/^\d/.test(part) ? `app${part}` : part))
    .join('.');
  return `app.glowbom.${slug || 'myapp'}`;
}

interface ProjectHistoryEntry {
  path: string;
  name: string;
  version: string;
  lastOpenedAt: string;
}

interface HistoryViewEntry extends Omit<OpenCodeProjectHistoryEntry, 'attachments'> {
  attachments: OpenCodeProjectHistoryAttachment[];
  optimistic?: boolean;
}

function IDEQuickActionIcon({ ide }: { ide: OpenCodeIDE }) {
  switch (ide) {
    case 'finder':
      return (
        <svg aria-hidden="true" fill="none" viewBox="0 0 16 16">
          <path
            d="M1.75 4.25a1 1 0 0 1 1-1h3.1l1.2 1.5h6.2a1 1 0 0 1 1 1v5.5a1.5 1.5 0 0 1-1.5 1.5h-10.5a1.5 1.5 0 0 1-1.5-1.5v-7Z"
            stroke="currentColor"
            strokeLinecap="round"
            strokeLinejoin="round"
            strokeWidth="1.3"
          />
          <path d="M2 6h12" stroke="currentColor" strokeLinecap="round" strokeWidth="1.3" />
        </svg>
      );
    case 'xcode':
      return (
        <svg aria-hidden="true" fill="none" viewBox="0 0 16 16">
          <path
            d="M9.66 2.03c-.48.06-1.04.39-1.43.83-.34.38-.63.94-.56 1.5.53.04 1.08-.27 1.45-.71.39-.46.63-1.01.54-1.62Z"
            fill="currentColor"
          />
          <path
            d="M12.94 10.88c-.22.51-.49.98-.83 1.43-.48.64-.88 1.36-1.64 1.37-.66.02-.87-.39-1.76-.39-.88 0-1.13.37-1.74.4-.73.03-1.29-.71-1.78-1.35-1.38-1.8-1.53-4.95-.34-6.73.58-.88 1.53-1.44 2.45-1.44.77 0 1.49.44 1.94.44.44 0 1.27-.54 2.14-.46.36.02 1.38.15 2.04 1.13-.05.04-1.22.73-1.21 2.19.02 1.74 1.53 2.32 1.55 2.33-.01.05-.24.75-.82 1.48Z"
            fill="currentColor"
          />
        </svg>
      );
    case 'android-studio':
      return (
        <svg aria-hidden="true" fill="none" viewBox="0 0 16 16">
          <path d="m5.14 3.28-.92-1.33m6.64 1.33.92-1.33" stroke="currentColor" strokeLinecap="round" strokeWidth="1.15" />
          <path
            d="M4.2 5.88A3.82 3.82 0 0 1 8 3.64a3.82 3.82 0 0 1 3.8 2.24H4.2Z"
            fill="currentColor"
          />
          <circle cx="6.56" cy="4.9" fill="#f7fcfa" r=".42" />
          <circle cx="9.44" cy="4.9" fill="#f7fcfa" r=".42" />
          <rect fill="currentColor" height="5.14" rx="1.35" width="7.08" x="4.46" y="6.28" />
          <rect fill="currentColor" height="2.82" rx=".55" width=".96" x="3.28" y="6.78" />
          <rect fill="currentColor" height="2.82" rx=".55" width=".96" x="11.76" y="6.78" />
          <rect fill="currentColor" height="2.62" rx=".55" width=".96" x="5.68" y="10.72" />
          <rect fill="currentColor" height="2.62" rx=".55" width=".96" x="9.36" y="10.72" />
        </svg>
      );
    case 'vscode':
      return (
        <svg aria-hidden="true" fill="none" viewBox="0 0 16 16">
          <path
            d="m10.8 2.2 3 1.4v8.8l-3 1.4-5.9-5.1 5.9-6.5Z"
            stroke="currentColor"
            strokeLinejoin="round"
            strokeWidth="1.2"
          />
          <path d="M5 5.6 2.3 3.5 1.1 4.7 3.8 8l-2.7 3.3 1.2 1.2L5 10.4" stroke="currentColor" strokeLinejoin="round" strokeWidth="1.2" />
        </svg>
      );
    default:
      return null;
  }
}

function isMultiSelect(prompt: string, inputType?: string): boolean {
  const promptLower = prompt.toLowerCase();
  const typeLower = (inputType || '').toLowerCase();

  if (typeLower.includes('multi') || typeLower.includes('checkbox')) {
    return true;
  }

  return (
    promptLower.includes('select all') ||
    promptLower.includes('choose all') ||
    promptLower.includes('select one or more') ||
    promptLower.includes('choose one or more') ||
    promptLower.includes('multiple')
  );
}

function toggleSelection(current: string[], value: string, multiSelect: boolean): string[] {
  if (!multiSelect) {
    return [value];
  }

  if (current.includes(value)) {
    return current.filter((item) => item !== value);
  }

  return [...current, value];
}

function targetSummary(project: OpenCodeProject | null): Array<{ id: string; stack: string; outputDir: string }> {
  if (!project) {
    return [];
  }

  return Object.entries(project.targets).map(([id, target]) => ({
    id,
    stack: target.stack || 'unknown',
    outputDir: target.outputDir || id,
  }));
}

function formatCredentialType(value?: string): string {
  switch ((value || '').toLowerCase()) {
    case 'oauth':
      return 'ChatGPT OAuth credential';
    case 'api':
      return 'OpenAI API credential';
    case 'none':
      return 'None';
    default:
      return 'Unknown';
  }
}

function formatAuthMode(value?: string): string {
  switch ((value || '').toLowerCase()) {
    case 'api-key':
      return 'API Key';
    case 'codex-jwt':
      return 'ChatGPT Login';
    case 'opencode-config':
      return 'OpenCode Config';
    default:
      return 'Unknown';
  }
}

function formatAttachmentSize(sizeBytes: number): string {
  if (!Number.isFinite(sizeBytes) || sizeBytes <= 0) {
    return '0 B';
  }
  if (sizeBytes < 1024) {
    return `${sizeBytes} B`;
  }

  const kib = sizeBytes / 1024;
  if (kib < 1024) {
    return `${Math.round(kib)} KB`;
  }

  const mib = kib / 1024;
  if (mib < 1024) {
    return `${mib.toFixed(1)} MB`;
  }

  return `${(mib / 1024).toFixed(1)} GB`;
}

function compactPathLabel(path: string): string {
  const trimmed = path.trim();
  if (!trimmed) {
    return 'No project';
  }

  const parts = trimmed.split(/[\\/]/).filter((part) => part.length > 0);
  return parts[parts.length - 1] || trimmed;
}

function compactHistoryText(value: string, maxLength = 180, fallback = 'No saved instructions.') {
  const collapsed = value.trim().replace(/\s+/g, ' ');
  if (!collapsed) {
    return fallback;
  }
  if (collapsed.length <= maxLength) {
    return collapsed;
  }
  return `${collapsed.slice(0, maxLength).trimEnd()}...`;
}

function formatHistoryTimestamp(value: string): string {
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) {
    return value || 'Unknown time';
  }

  return new Intl.DateTimeFormat(undefined, {
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  }).format(parsed);
}

function inferHistoryAttachmentMediaType(mimeType?: string, fileName?: string): string {
  const normalizedMimeType = (mimeType || '').trim().toLowerCase();
  if (normalizedMimeType.startsWith('image/')) {
    return 'screenshot';
  }
  if (normalizedMimeType.startsWith('video/')) {
    return 'video';
  }
  if (normalizedMimeType.startsWith('audio/')) {
    return 'audio';
  }
  if (normalizedMimeType.startsWith('text/')) {
    return 'document';
  }

  const extension = (fileName || '').split('.').pop()?.toLowerCase() || '';
  if (['md', 'txt', 'json', 'yaml', 'yml', 'xml', 'csv', 'pdf'].includes(extension)) {
    return 'document';
  }

  return 'other';
}

function normalizeHistoryEntries(entries: OpenCodeProjectHistoryEntry[]): HistoryViewEntry[] {
  return entries.map((entry) => ({
    ...entry,
    attachments: (entry.attachments || []).map((attachment) => ({
      ...attachment,
      filename: attachment.filename || attachment.name,
      mediaType: attachment.mediaType || inferHistoryAttachmentMediaType(attachment.mimeType, attachment.filename || attachment.name),
      relativePath: attachment.relativePath || attachment.path,
    })),
  }));
}

function historyAttachmentCountLabel(count: number): string {
  return `${count} file${count === 1 ? '' : 's'}`;
}

function normalizeProjectHistory(raw: unknown): ProjectHistoryEntry[] {
  if (!Array.isArray(raw)) {
    return [];
  }

  const seen = new Set<string>();
  const entries: ProjectHistoryEntry[] = [];

  for (const item of raw) {
    if (!item || typeof item !== 'object') {
      continue;
    }

    const path = typeof item.path === 'string' ? item.path.trim() : '';
    if (!path || seen.has(path)) {
      continue;
    }

    seen.add(path);
    entries.push({
      path,
      name:
        typeof item.name === 'string' && item.name.trim().length > 0
          ? item.name.trim()
          : compactPathLabel(path),
      version: typeof item.version === 'string' ? item.version.trim() : '',
      lastOpenedAt: typeof item.lastOpenedAt === 'string' ? item.lastOpenedAt : '',
    });
  }

  return entries
    .sort((left, right) => right.lastOpenedAt.localeCompare(left.lastOpenedAt))
    .slice(0, 10);
}

function upsertProjectHistory(previous: ProjectHistoryEntry[], entry: ProjectHistoryEntry): ProjectHistoryEntry[] {
  return normalizeProjectHistory([entry, ...previous.filter((item) => item.path !== entry.path)]);
}

function inferProviderFromCustomModel(customModel: string): ProviderID | null {
  const trimmed = customModel.trim();
  const slashIndex = trimmed.indexOf('/');
  if (slashIndex <= 0) {
    return null;
  }

  const providerId = trimmed.slice(0, slashIndex) as ProviderID;
  if (!providerDefinition(providerId)) {
    return null;
  }

  return providerId;
}

function defaultModelOptionValue(dynamicOpenAIModels: OpenAIModelsResponseModel[] = []): string {
  const firstGroup = modelCatalogGroups(dynamicOpenAIModels)[0];
  if (!firstGroup || firstGroup.models.length === 0) {
    return '';
  }

  const firstModel = firstGroup.models[0];
  if (!firstModel) {
    return '';
  }
  return encodeModelOptionValue(firstGroup.provider.id, firstModel.id);
}

function ideActionFor(actions: OpenCodeProjectIDEAction[], ide: OpenCodeIDE): OpenCodeProjectIDEAction | undefined {
  return actions.find((item) => item.ide === ide);
}

function isUnsupportedNativePickerError(error: unknown): boolean {
  if (error instanceof ApiError && error.status === 501) {
    return true;
  }

  return toErrorMessage(error, '').toLowerCase().includes('only available on');
}

export default function App() {
  const topbarLogoSrc = `${import.meta.env.BASE_URL}logo-svg.svg`;
  const [health, setHealth] = useState<OpenCodeHealthResponse | null>(null);
  const [authStatus, setAuthStatus] = useState<OpenCodeAuthStatus | null>(null);
  const [healthError, setHealthError] = useState<string | null>(null);
  const [authError, setAuthError] = useState<string | null>(null);
  const [isCheckingSetup, setIsCheckingSetup] = useState(false);

  const [projectPath, setProjectPath] = useState('');
  const [projectEnvelope, setProjectEnvelope] = useState<OpenCodeProjectEnvelope | null>(null);
  const [projectError, setProjectError] = useState<string | null>(null);
  const [isLoadingProject, setIsLoadingProject] = useState(false);
  const [isPickingFolder, setIsPickingFolder] = useState(false);
  const [folderPickerInfo, setFolderPickerInfo] = useState<string | null>(null);
  const [folderPickerWarning, setFolderPickerWarning] = useState<string | null>(null);
  const [instructionAttachments, setInstructionAttachments] = useState<OpenCodeInstructionPickedFile[]>([]);
  const [isPickingInstructionFiles, setIsPickingInstructionFiles] = useState(false);
  const [instructionPickerInfo, setInstructionPickerInfo] = useState<string | null>(null);
  const [instructionPickerWarning, setInstructionPickerWarning] = useState<string | null>(null);
  const [projectHistory, setProjectHistory] = useState<ProjectHistoryEntry[]>([]);
  const [historyEntries, setHistoryEntries] = useState<HistoryViewEntry[]>([]);
  const [isLoadingHistory, setIsLoadingHistory] = useState(false);
  const [historyError, setHistoryError] = useState<string | null>(null);
  const [historyInfo, setHistoryInfo] = useState<string | null>(null);
  const [optimisticHistoryEntry, setOptimisticHistoryEntry] = useState<HistoryViewEntry | null>(null);
  const [ideStatus, setIdeStatus] = useState<OpenCodeProjectIDEStatusResponse | null>(null);
  const [isLoadingIdeStatus, setIsLoadingIdeStatus] = useState(false);
  const [ideStatusError, setIdeStatusError] = useState<string | null>(null);
  const [ideOpenInfo, setIdeOpenInfo] = useState<string | null>(null);
  const [ideOpenError, setIdeOpenError] = useState<string | null>(null);
  const [openingIDE, setOpeningIDE] = useState<OpenCodeIDE | null>(null);
  const [isRenamingProject, setIsRenamingProject] = useState(false);
  const [renameValue, setRenameValue] = useState('');
  const [renameError, setRenameError] = useState<string | null>(null);

  const [settingsBundleID, setSettingsBundleID] = useState('');
  const [settingsDisplayName, setSettingsDisplayName] = useState('');
  const [settingsBuildNumber, setSettingsBuildNumber] = useState('');
  const [settingsVersion, setSettingsVersion] = useState('');
  const [isSavingSettings, setIsSavingSettings] = useState(false);
  const [settingsInfo, setSettingsInfo] = useState<string | null>(null);
  const [settingsError, setSettingsError] = useState<string | null>(null);
  const [iconPrompt, setIconPrompt] = useState('');
  const [isGeneratingIcon, setIsGeneratingIcon] = useState(false);
  const [iconPreview, setIconPreview] = useState<string | null>(null);
  const [iconInfo, setIconInfo] = useState<string | null>(null);
  const [iconError, setIconError] = useState<string | null>(null);
  const [iconReferenceImage, setIconReferenceImage] = useState<string | null>(null);

  const [providerKeys, setProviderKeys] = useState<ProviderKeyState>(loadProviderKeys);
  const [imageProviderKeys, setImageProviderKeys] = useState<ImageProviderKeyState>(loadImageProviderKeys);
  const [imageSource, setImageSourceRaw] = useState(() => validateImageSource(loadImageSource()));
  const setImageSource = (value: string | ((prev: string) => string)) => {
    setImageSourceRaw((prev) => {
      const next = normalizeImageSource(typeof value === 'function' ? value(prev) : value);
      saveImageSource(next);
      return next;
    });
  };
  const [credentialMode, setCredentialModeRaw] = useState<CredentialMode>(loadCredentialMode);
  const setCredentialMode = (mode: CredentialMode) => {
    setCredentialModeRaw(mode);
    saveCredentialMode(mode);
  };
  const [authProvider, setAuthProvider] = useState<AuthProviderID>('chatgpt');
  const [isUpdatingAuthConnection, setIsUpdatingAuthConnection] = useState(false);
  const [authConnectionInfo, setAuthConnectionInfo] = useState<string | null>(null);
  const [authConnectionError, setAuthConnectionError] = useState<string | null>(null);

  const [dynamicOpenAIModels, setDynamicOpenAIModels] = useState<OpenAIModelsResponseModel[]>([]);
  const [isLoadingOpenAIModels, setIsLoadingOpenAIModels] = useState(false);
  const [openAIModelsInfo, setOpenAIModelsInfo] = useState<string | null>(null);
  const [openAIModelsWarning, setOpenAIModelsWarning] = useState<string | null>(null);
  const [openCodeConfigProviders, setOpenCodeConfigProviders] = useState<OpenCodeAvailableProvider[]>([]);
  const [selectedOpenCodeModel, setSelectedOpenCodeModel] = useState(OPENCODE_RECOMMENDED_MODEL_VALUE);
  const [isLoadingOpenCodeModels, setIsLoadingOpenCodeModels] = useState(false);
  const [openCodeModelsInfo, setOpenCodeModelsInfo] = useState<string | null>(null);
  const [openCodeModelsWarning, setOpenCodeModelsWarning] = useState<string | null>(null);

  const [selectedModelOption, setSelectedModelOption] = useState(() => defaultModelOptionValue());
  const [customModel, setCustomModel] = useState('');
  const [instructions, setInstructions] = useState(DEFAULT_BUILD_INSTRUCTIONS);
  const [formError, setFormError] = useState<string | null>(null);

  const [simpleAnswerText, setSimpleAnswerText] = useState('');
  const [simpleCustomAnswer, setSimpleCustomAnswer] = useState('');
  const [simpleSelectedOptions, setSimpleSelectedOptions] = useState<string[]>([]);

  const [nestedTextById, setNestedTextById] = useState<Record<string, string>>({});
  const [nestedCustomById, setNestedCustomById] = useState<Record<string, string>>({});
  const [nestedSelectionsById, setNestedSelectionsById] = useState<Record<string, string[]>>({});

  const consoleRef = useRef<HTMLDivElement | null>(null);
  const [autoScrollEnabled, setAutoScrollEnabled] = useState(true);
  const [isProjectPickerOpen, setIsProjectPickerOpen] = useState(false);
  const [isContextOpen, setIsContextOpen] = useState(false);
  const [isSettingsOpen, setIsSettingsOpen] = useState(false);

  const refine = useRefineRun();
  const previousRefineStatusRef = useRef(refine.status);

  const activeProject = projectEnvelope?.project ?? null;
  const targetRows = useMemo(() => targetSummary(activeProject), [activeProject]);
  const [selectedTargets, setSelectedTargets] = useState<string[]>(loadSelectedTargets);
  const modelGroups = useMemo(() => modelCatalogGroups(dynamicOpenAIModels), [dynamicOpenAIModels]);
  const isAuthMode = credentialMode === 'auth';
  const isApiKeyMode = credentialMode === 'api-key';
  const isOpenCodeConfigMode = credentialMode === 'opencode-config';
  const effectiveOpenAIAuthMode: OpenAIAuthMode = isAuthMode
    ? 'codex-jwt'
    : isOpenCodeConfigMode
      ? 'opencode-config'
      : 'api-key';
  const isChatGPTConnected =
    authStatus?.openaiCredentialType === 'oauth' || authStatus?.cachedGlowbomAuthMode === 'codex-jwt';
  const authProviderConnected = authProvider === 'chatgpt' && isChatGPTConnected;
  const selectedImageProvider =
    IMAGE_PROVIDERS.find((provider) => provider.source === imageSource) || DEFAULT_IMAGE_PROVIDER;
  const selectedImageProviderHasKey = Boolean(
    imageProviderKeys[selectedImageProvider.keyField].trim(),
  );

  const visibleModelGroups = useMemo(() => {
    if (isAuthMode) {
      return modelGroups.filter((group) => group.provider.id === 'openai');
    }
    if (isApiKeyMode) {
      return modelGroups;
    }
    return [];
  }, [isApiKeyMode, isAuthMode, modelGroups]);

  const allOptionValues = useMemo(
    () =>
      visibleModelGroups.flatMap((group) =>
        group.models.map((model) => encodeModelOptionValue(group.provider.id, model.id)),
      ),
    [visibleModelGroups],
  );

  const decodedSelection = useMemo(() => decodeModelOptionValue(selectedModelOption), [selectedModelOption]);

  const resolvedSelection = useMemo(() => {
    if (!decodedSelection) {
      return null;
    }
    return resolveRefineModel(decodedSelection.providerId, decodedSelection.modelId);
  }, [decodedSelection]);

  const selectedProvider = resolvedSelection?.providerId || null;
  const selectedProviderDefinition = selectedProvider ? providerDefinition(selectedProvider) : undefined;
  const selectedProviderKeyField = selectedProviderDefinition?.keyField;

  const runLogs = useMemo(() => {
    if (!refine.partialLine) {
      return refine.logs;
    }
    return [...refine.logs, refine.partialLine];
  }, [refine.logs, refine.partialLine]);

  const setupStatusText = health?.healthy ? 'Backend connected' : health ? 'Backend unavailable' : 'Checking backend';
  const shouldFetchOpenAIModels = isAuthMode && selectedProvider === 'openai';
  const ideActions = useMemo(() => ideStatus?.actions ?? [], [ideStatus?.actions]);
  const finderAction = useMemo(() => ideActionFor(ideActions, 'finder'), [ideActions]);
  const xcodeAction = useMemo(() => ideActionFor(ideActions, 'xcode'), [ideActions]);
  const androidStudioAction = useMemo(() => ideActionFor(ideActions, 'android-studio'), [ideActions]);
  const vscodeAction = useMemo(() => ideActionFor(ideActions, 'vscode'), [ideActions]);
  const selectedProjectPath = projectPath.trim();
  const attachmentCount = instructionAttachments.length;
  const recommendedOpenCodeModel = useMemo(
    () => findChatGPTRecommendedModel(authStatus?.openaiCredentialType, openCodeConfigProviders),
    [authStatus?.openaiCredentialType, openCodeConfigProviders],
  );
  const resolvedOpenCodeModelValue = useMemo(
    () => resolveOpenCodeModelValue(selectedOpenCodeModel, recommendedOpenCodeModel),
    [recommendedOpenCodeModel, selectedOpenCodeModel],
  );
  const selectedOpenCodeModelLabel = useMemo(() => {
    if (selectedOpenCodeModel === OPENCODE_RECOMMENDED_MODEL_VALUE) {
      return recommendedOpenCodeModel?.fullLabel || 'OpenCode default';
    }
    if (selectedOpenCodeModel === OPENCODE_DEFAULT_MODEL_VALUE) {
      return 'OpenCode default';
    }

    for (const provider of openCodeConfigProviders) {
      const matchingModel = provider.models.find((model) => `${provider.id}/${model.id}` === selectedOpenCodeModel);
      if (matchingModel) {
        return `${provider.displayName || provider.id}: ${matchingModel.displayName || matchingModel.id}`;
      }
    }

    return selectedOpenCodeModel;
  }, [openCodeConfigProviders, recommendedOpenCodeModel, selectedOpenCodeModel]);
  const recommendedOpenCodeModelOptionLabel = recommendedOpenCodeModel
    ? `Recommended: ${recommendedOpenCodeModel.modelLabel} via ChatGPT`
    : 'OpenCode default';
  const systemNeedsAttention = Boolean(
    healthError || authError || health?.healthy === false || authStatus?.serverRunning === false,
  );
  const systemStatusSummary = useMemo(() => {
    if (healthError) {
      return 'Health check failed';
    }
    if (!health && !authStatus && !authError) {
      return 'Checking setup';
    }
    if (health?.healthy === false) {
      return 'Backend needs attention';
    }
    if (authError) {
      return 'Auth check failed';
    }
    if (authStatus?.serverRunning === false) {
      return 'Agent server stopped';
    }
    return 'Local backend ready';
  }, [authError, authStatus, health, healthError]);
  const agentStatusSummary = useMemo(() => {
    if (isOpenCodeConfigMode) {
      return selectedOpenCodeModelLabel;
    }

    if (customModel.trim()) {
      return customModel.trim();
    }

    if (resolvedSelection) {
      return resolvedSelection.label;
    }

    if (isAuthMode) {
      return isChatGPTConnected ? 'ChatGPT connected' : 'Connect ChatGPT';
    }

    return 'Choose a model';
  }, [
    customModel,
    isAuthMode,
    isChatGPTConnected,
    isOpenCodeConfigMode,
    selectedOpenCodeModelLabel,
    resolvedSelection,
  ]);
  const imageProviderStatusSummary = selectedImageProviderHasKey
    ? selectedImageProvider.label
    : `${selectedImageProvider.label}: add key`;
  const projectButtonLabel = activeProject?.name || (selectedProjectPath ? compactPathLabel(selectedProjectPath) : 'Project');
  const contextButtonLabel = attachmentCount > 0 ? `Context ${attachmentCount}` : 'Context';

  const refreshProjectHistory = useCallback(
    async (
      pathOverride?: string,
      options: {
        silent?: boolean;
        clearOptimisticOnSuccess?: boolean;
      } = {},
    ) => {
      const trimmedPath = (pathOverride ?? projectPath).trim();
      if (!trimmedPath) {
        setHistoryEntries([]);
        if (!options.silent) {
          setHistoryError(null);
          setIsLoadingHistory(false);
        }
        return false;
      }

      if (!options.silent) {
        setIsLoadingHistory(true);
        setHistoryError(null);
      }

      try {
        const response = await openCodeApi.getProjectHistory(trimmedPath);
        if (!response.success) {
          throw new Error(response.error || 'Failed to load build history.');
        }

        setHistoryEntries(normalizeHistoryEntries(response.entries || []));
        setHistoryError(null);
        if (options.clearOptimisticOnSuccess) {
          setOptimisticHistoryEntry(null);
        }
        return true;
      } catch (error) {
        if (!options.silent) {
          setHistoryError(toErrorMessage(error, 'Failed to load build history.'));
        }
        return false;
      } finally {
        if (!options.silent) {
          setIsLoadingHistory(false);
        }
      }
    },
    [projectPath],
  );

  useEffect(() => {
    if (allOptionValues.length === 0) {
      if (selectedModelOption !== '') {
        setSelectedModelOption('');
      }
      return;
    }

    if (decodedSelection) {
      const normalizedSelection = encodeModelOptionValue(decodedSelection.providerId, decodedSelection.modelId);
      if (normalizedSelection !== selectedModelOption && allOptionValues.includes(normalizedSelection)) {
        setSelectedModelOption(normalizedSelection);
        return;
      }
    }

    if (!selectedModelOption || !allOptionValues.includes(selectedModelOption)) {
      setSelectedModelOption(allOptionValues[0] || '');
    }
  }, [allOptionValues, decodedSelection, selectedModelOption]);

  const refreshSetup = async () => {
    setIsCheckingSetup(true);
    setHealthError(null);
    setAuthError(null);

    try {
      const healthResult = await openCodeApi.getHealth();
      setHealth(healthResult);
    } catch (error) {
      setHealth(null);
      setHealthError(toErrorMessage(error, 'Failed to check backend health.'));
    }

    try {
      const authResult = await openCodeApi.getAuthStatus();
      setAuthStatus(authResult);
    } catch (error) {
      setAuthStatus(null);
      setAuthError(toErrorMessage(error, 'Failed to fetch auth status.'));
    }

    setIsCheckingSetup(false);
  };

  const refreshIDEStatus = async (path: string) => {
    const trimmedPath = path.trim();
    if (!trimmedPath) {
      setIdeStatus(null);
      setIdeStatusError(null);
      return;
    }

    setIsLoadingIdeStatus(true);
    setIdeStatusError(null);

    try {
      const response = await openCodeApi.getProjectIDEStatus(trimmedPath);
      if (!response.success) {
        throw new Error(response.error || 'Failed to inspect project structure.');
      }
      setIdeStatus(response);
    } catch (error) {
      setIdeStatus(null);
      setIdeStatusError(toErrorMessage(error, 'Failed to inspect IDE project folders.'));
    } finally {
      setIsLoadingIdeStatus(false);
    }
  };

  useEffect(() => {
    try {
      const raw = readMigratedLocalStorage(PROJECT_HISTORY_STORAGE_KEY, LEGACY_PROJECT_HISTORY_STORAGE_KEY);
      if (!raw) {
        return;
      }
      const history = normalizeProjectHistory(JSON.parse(raw));
      const lastProject = history[0];
      setProjectHistory(history);
      if (lastProject?.path) {
        setProjectPath((previous) => previous.trim() || lastProject.path);
        void loadProject(lastProject.path);
      }
    } catch {
      setProjectHistory([]);
    }
  }, []);

  useEffect(() => {
    void refreshSetup();
  }, []);

  useEffect(() => {
    const container = consoleRef.current;
    if (!container || !autoScrollEnabled) {
      return;
    }

    container.scrollTop = container.scrollHeight;
  }, [
    autoScrollEnabled,
    runLogs,
    refine.pendingQuestion,
    refine.pendingPermission,
    refine.pendingMediaApproval,
  ]);

  useEffect(() => {
    setSimpleAnswerText('');
    setSimpleCustomAnswer('');
    setSimpleSelectedOptions([]);
    setNestedTextById({});
    setNestedCustomById({});
    setNestedSelectionsById({});
  }, [refine.pendingQuestion?.id]);

  useEffect(() => {
    let cancelled = false;

    if (!shouldFetchOpenAIModels) {
      setDynamicOpenAIModels([]);
      setOpenAIModelsInfo(null);
      setOpenAIModelsWarning(null);
      setIsLoadingOpenAIModels(false);
      return;
    }

    if (!isChatGPTConnected) {
      setDynamicOpenAIModels([]);
      setOpenAIModelsInfo(null);
      setOpenAIModelsWarning('Connect ChatGPT to load live OpenAI models.');
      setIsLoadingOpenAIModels(false);
      return;
    }

    setIsLoadingOpenAIModels(true);
    setOpenAIModelsInfo(null);
    setOpenAIModelsWarning(null);

    void openCodeApi
      .fetchOpenAIModels({
        projectPath: projectPath.trim() || undefined,
        openaiAuthMode: effectiveOpenAIAuthMode,
      })
      .then((response) => {
        if (cancelled) {
          return;
        }

        setDynamicOpenAIModels(response.models || []);
        setOpenAIModelsInfo(`Loaded ${response.models.length} OpenAI models from your local backend.`);

        if (response.debug?.usedFallbackAllowlist) {
          setOpenAIModelsWarning('Dynamic model catalog returned fallback defaults. You can still continue.');
        } else {
          setOpenAIModelsWarning(null);
        }
      })
      .catch((error) => {
        if (cancelled) {
          return;
        }

        setDynamicOpenAIModels([]);
        setOpenAIModelsInfo(null);
        setOpenAIModelsWarning(
          `Could not load dynamic OpenAI models. Showing fallback defaults. ${toErrorMessage(error, '')}`.trim(),
        );
      })
      .finally(() => {
        if (!cancelled) {
          setIsLoadingOpenAIModels(false);
        }
      });

    return () => {
      cancelled = true;
    };
  }, [
    effectiveOpenAIAuthMode,
    projectPath,
    isChatGPTConnected,
    shouldFetchOpenAIModels,
  ]);

  useEffect(() => {
    let cancelled = false;

    if (!isOpenCodeConfigMode) {
      setOpenCodeConfigProviders([]);
      setOpenCodeModelsInfo(null);
      setOpenCodeModelsWarning(null);
      setIsLoadingOpenCodeModels(false);
      setSelectedOpenCodeModel(OPENCODE_RECOMMENDED_MODEL_VALUE);
      return;
    }

    setIsLoadingOpenCodeModels(true);
    setOpenCodeModelsInfo(null);
    setOpenCodeModelsWarning(null);

    void openCodeApi
      .fetchOpenCodeAvailableModels({
        projectPath: projectPath.trim() || undefined,
        openaiAuthMode: effectiveOpenAIAuthMode,
        openaiKey: providerKeys.openaiKey.trim() || undefined,
        anthropicKey: providerKeys.anthropicKey.trim() || undefined,
        geminiKey: providerKeys.geminiKey.trim() || undefined,
        fireworksKey: providerKeys.fireworksKey.trim() || undefined,
        openrouterKey: providerKeys.openrouterKey.trim() || undefined,
        opencodeZenKey: providerKeys.opencodeZenKey.trim() || undefined,
        xaiKey: providerKeys.xaiKey.trim() || undefined,
      })
      .then((response) => {
        if (cancelled) {
          return;
        }

        const providers = response.providers || [];
        setOpenCodeConfigProviders(providers);
        const modelCount = providers.reduce((sum, provider) => sum + provider.models.length, 0);
        setOpenCodeModelsInfo(
          `Loaded ${providers.length} provider${providers.length === 1 ? '' : 's'} and ${modelCount} model${modelCount === 1 ? '' : 's'} from OpenCode config.`,
        );
        if (modelCount === 0) {
          setOpenCodeModelsWarning('OpenCode returned no models. Configure a provider in OpenCode, then refresh Settings.');
        } else {
          setOpenCodeModelsWarning(null);
        }
      })
      .catch((error) => {
        if (cancelled) {
          return;
        }

        setOpenCodeConfigProviders([]);
        setOpenCodeModelsInfo(null);
        setOpenCodeModelsWarning(
          `Could not load the OpenCode provider catalog. Check your OpenCode setup, then refresh Settings. ${toErrorMessage(error, '')}`.trim(),
        );
      })
      .finally(() => {
        if (!cancelled) {
          setIsLoadingOpenCodeModels(false);
        }
      });

    return () => {
      cancelled = true;
    };
  }, [
    effectiveOpenAIAuthMode,
    isOpenCodeConfigMode,
    projectPath,
    providerKeys.anthropicKey,
    providerKeys.fireworksKey,
    providerKeys.geminiKey,
    providerKeys.openaiKey,
    providerKeys.opencodeZenKey,
    providerKeys.openrouterKey,
    providerKeys.xaiKey,
  ]);

  useEffect(() => {
    if (!isOpenCodeConfigMode) {
      return;
    }

    if (
      selectedOpenCodeModel === OPENCODE_RECOMMENDED_MODEL_VALUE ||
      selectedOpenCodeModel === OPENCODE_DEFAULT_MODEL_VALUE
    ) {
      return;
    }

    const exists = openCodeConfigProviders.some((provider) =>
      provider.models.some((model) => `${provider.id}/${model.id}` === selectedOpenCodeModel),
    );

    if (!exists) {
      setSelectedOpenCodeModel(OPENCODE_DEFAULT_MODEL_VALUE);
    }
  }, [isOpenCodeConfigMode, openCodeConfigProviders, selectedOpenCodeModel]);

  useEffect(() => {
    if (!activeProject) return;
    const name = activeProject.name || '';
    setSettingsBundleID(activeProject.bundleID || suggestBundleID(name));
    setSettingsDisplayName(activeProject.displayName || name);
    setSettingsBuildNumber(activeProject.buildNumber || '1');
    setSettingsVersion(activeProject.version || '1.0');
    setSettingsInfo(null);
    setSettingsError(null);
    setIconInfo(null);
    setIconError(null);
    setIconReferenceImage(null);

    // Load existing icon
    const trimmedPath = projectPath.trim();
    if (trimmedPath) {
      openCodeApi.getProjectIcon(trimmedPath).then((result) => {
        setIconPreview(result.exists && result.image ? result.image : null);
      }).catch(() => {
        setIconPreview(null);
      });
    } else {
      setIconPreview(null);
    }
  }, [activeProject, projectPath]);

  const toggleTarget = useCallback((targetId: string) => {
    setSelectedTargets((prev) => {
      if (prev.includes(targetId) && prev.length === 1) return prev;
      const next = prev.includes(targetId)
        ? prev.filter((id) => id !== targetId)
        : [...prev, targetId];
      saveSelectedTargets(next);
      return next;
    });
  }, []);

  useEffect(() => {
    if (!isProjectPickerOpen || refine.isRunning || !activeProject || !selectedProjectPath) {
      return;
    }

    void refreshProjectHistory(selectedProjectPath);
  }, [activeProject, isProjectPickerOpen, refreshProjectHistory, refine.isRunning, selectedProjectPath]);

  useEffect(() => {
    const previousStatus = previousRefineStatusRef.current;
    previousRefineStatusRef.current = refine.status;

    if (previousStatus !== 'running' || refine.status === 'running') {
      return;
    }

    setOptimisticHistoryEntry((previous) => {
      if (!previous) {
        return previous;
      }

      return {
        ...previous,
        status: refine.status as OpenCodeHistoryStatus,
        outputSummary: refine.summary || previous.outputSummary,
      };
    });
    if (!selectedProjectPath) {
      return;
    }

    void refreshProjectHistory(selectedProjectPath, { clearOptimisticOnSuccess: true });
  }, [refine.status, refine.summary, refreshProjectHistory, selectedProjectPath]);

  const updateProviderKey = (field: keyof ProviderKeyState, value: string) => {
    setProviderKeys((previous) => {
      const next = { ...previous, [field]: value };
      saveProviderKeys(next);
      return next;
    });
  };

  const updateImageProviderKey = (field: keyof ImageProviderKeyState, value: string) => {
    setImageProviderKeys((previous) => {
      const next = { ...previous, [field]: value };
      saveImageProviderKeys(next);
      return next;
    });
  };

  const toggleProjectPicker = () => {
    setIsProjectPickerOpen((previous) => {
      const next = !previous;
      if (next) {
        setIsContextOpen(false);
        setIsSettingsOpen(false);
      }
      return next;
    });
  };

  const toggleContextPanel = () => {
    setIsContextOpen((previous) => {
      const next = !previous;
      if (next) {
        setIsProjectPickerOpen(false);
        setIsSettingsOpen(false);
      }
      return next;
    });
  };

  const toggleSettingsPanel = () => {
    setIsSettingsOpen((previous) => {
      const next = !previous;
      if (next) {
        setIsProjectPickerOpen(false);
        setIsContextOpen(false);
      }
      return next;
    });
  };

  const clearProjectSelection = () => {
    setProjectPath('');
    setProjectEnvelope(null);
    setProjectError(null);
    setFolderPickerInfo(null);
    setFolderPickerWarning(null);
    setIdeStatus(null);
    setIdeStatusError(null);
    setIdeOpenInfo(null);
    setIdeOpenError(null);
    setHistoryEntries([]);
    setHistoryError(null);
    setHistoryInfo(null);
    setOptimisticHistoryEntry(null);
    setIsProjectPickerOpen(false);
  };

  const onConsoleScroll = () => {
    const element = consoleRef.current;
    if (!element) {
      return;
    }

    const distanceFromBottom = element.scrollHeight - element.scrollTop - element.clientHeight;
    setAutoScrollEnabled(distanceFromBottom < 24);
  };

  const openFolderPicker = async () => {
    setFolderPickerInfo(null);
    setFolderPickerWarning(null);
    setIsPickingFolder(true);

    try {
      const response = await openCodeApi.pickProjectFolder();

      if (response.success && response.path) {
        setProjectPath(response.path);
        setIsPickingFolder(false);
        const projectLoaded = await loadProject(response.path);
        if (projectLoaded) {
          setFolderPickerInfo('Project folder loaded locally.');
        }
        return;
      }

      if (response.success && response.canceled) {
        setFolderPickerInfo('Folder selection canceled.');
        return;
      }

      const apiMessage = (response.error || '').trim();
      if (apiMessage) {
        if (apiMessage.toLowerCase().includes('only available on')) {
          setFolderPickerWarning('This built-in folder picker is not available on this platform. Paste the project path manually.');
        } else {
          setFolderPickerWarning(`${apiMessage} Paste the project path manually if needed.`);
        }
        return;
      }
    } catch (error) {
      if (isUnsupportedNativePickerError(error)) {
        setFolderPickerWarning('This built-in folder picker is not available on this platform. Paste the project path manually.');
        return;
      }

      const message = toErrorMessage(error, 'unknown error').toLowerCase();
      if (message.includes('404') || message.includes('not found')) {
        setFolderPickerWarning(
          'Native folder picker API is unavailable. Restart the backend from latest code, then try again.',
        );
      } else {
        setFolderPickerWarning(
          `Native folder picker unavailable: ${toErrorMessage(error, 'unknown error')}. Paste the project path manually.`,
        );
      }
      return;
    } finally {
      setIsPickingFolder(false);
    }
  };

  const pickInstructionFiles = async () => {
    setInstructionPickerInfo(null);
    setInstructionPickerWarning(null);
    setIsPickingInstructionFiles(true);

    try {
      const response = await openCodeApi.pickInstructionFiles();
      if (response.success && response.canceled) {
        setInstructionPickerInfo('Attachment selection canceled.');
        return;
      }

      if (!response.success) {
        throw new Error(response.error || 'Failed to open local file picker.');
      }

      const pickedFiles = (response.files || []).filter((file) => file.path.trim().length > 0);
      if (pickedFiles.length === 0) {
        setInstructionPickerWarning('No files were selected.');
        return;
      }

      const oversizeFiles = pickedFiles.filter((file) => file.sizeBytes > MAX_INSTRUCTION_ATTACHMENT_BYTES);
      if (oversizeFiles.length > 0) {
        const first = oversizeFiles[0];
        if (first) {
          setInstructionPickerWarning(
            `"${first.name}" exceeds the 40MB limit (${formatAttachmentSize(first.sizeBytes)}). Remove it and try again.`,
          );
        }
      }

      setInstructionAttachments((previous) => {
        const merged = [...previous];
        for (const file of pickedFiles) {
          const exists = merged.some((entry) => entry.path === file.path);
          if (!exists) {
            merged.push(file);
          }
        }
        return merged;
      });

      const addedCount = pickedFiles.length;
      setInstructionPickerInfo(`Attached ${addedCount} local file${addedCount === 1 ? '' : 's'} for this build.`);
    } catch (error) {
      if (isUnsupportedNativePickerError(error)) {
        setInstructionPickerWarning('This built-in file picker is not available on this platform.');
        return;
      }

      const message = toErrorMessage(error, 'unknown error').toLowerCase();
      if (message.includes('404') || message.includes('not found')) {
        setInstructionPickerWarning(
          'Instruction file picker API is unavailable. Restart the backend from latest code, then try again.',
        );
      } else {
        setInstructionPickerWarning(
          `Could not open local file picker: ${toErrorMessage(error, 'unknown error')}.`,
        );
      }
    } finally {
      setIsPickingInstructionFiles(false);
    }
  };

  const loadProject = async (pathOverride?: string) => {
    const trimmedPath = (pathOverride ?? projectPath).trim();
    if (!trimmedPath) {
      setProjectError('Enter a local Glowbom project path first.');
      return false;
    }

    setProjectError(null);
    setFolderPickerInfo(null);
    setProjectEnvelope(null);
    setIdeStatus(null);
    setIdeStatusError(null);
    setIdeOpenInfo(null);
    setIdeOpenError(null);
    setHistoryEntries([]);
    setHistoryError(null);
    setHistoryInfo(null);
    setOptimisticHistoryEntry(null);
    setIsLoadingProject(true);

    try {
      const envelope = await openCodeApi.getProject(trimmedPath);
      if (!envelope.success || !envelope.project) {
        throw new Error(envelope.error || 'The provided path is not a valid Glowbom project.');
      }
      setProjectPath(trimmedPath);
      setProjectEnvelope(envelope);
      setProjectError(null);
      setProjectHistory((previous) => {
        const next = upsertProjectHistory(previous, {
          path: trimmedPath,
          name: envelope.project?.name?.trim() || compactPathLabel(trimmedPath),
          version: envelope.project?.version?.trim() || '',
          lastOpenedAt: new Date().toISOString(),
        });
        try {
          window.localStorage.setItem(PROJECT_HISTORY_STORAGE_KEY, JSON.stringify(next));
        } catch {}
        return next;
      });
      setIsProjectPickerOpen(false);
      void refreshProjectHistory(trimmedPath, { silent: true });
      await refreshIDEStatus(trimmedPath);
      return true;
    } catch (error) {
      setProjectError(toErrorMessage(error, 'Failed to load project.'));
      return false;
    } finally {
      setIsLoadingProject(false);
    }
  };

  const handleRenameProject = async (newName: string) => {
    const trimmed = newName.trim();
    if (!trimmed) {
      setRenameError('Name cannot be empty.');
      return;
    }
    const trimmedPath = projectPath.trim();
    if (!trimmedPath) return;

    setRenameError(null);
    try {
      const result = await openCodeApi.renameProject(trimmedPath, trimmed);
      if (result.success && result.project) {
        setProjectEnvelope((prev) =>
          prev ? { ...prev, project: result.project } : prev
        );
        // Update project history entry name
        setProjectHistory((prev) =>
          prev.map((entry) =>
            entry.path === trimmedPath ? { ...entry, name: trimmed } : entry
          )
        );
        // Auto-suggest display name and bundle ID when project name changes
        setSettingsDisplayName((prev) => prev ? prev : trimmed);
        setSettingsBundleID((prev) => prev ? prev : suggestBundleID(trimmed));
      } else {
        setRenameError(result.error || 'Failed to rename project.');
      }
    } catch (error) {
      setRenameError(toErrorMessage(error, 'Failed to rename project.'));
    } finally {
      setIsRenamingProject(false);
    }
  };

  const handleSaveProjectSettings = async () => {
    const trimmedPath = projectPath.trim();
    if (!trimmedPath) return;

    setIsSavingSettings(true);
    setSettingsInfo(null);
    setSettingsError(null);

    try {
      const result = await openCodeApi.updateProjectSettings({
        path: trimmedPath,
        bundleID: settingsBundleID,
        displayName: settingsDisplayName,
        buildNumber: settingsBuildNumber,
        version: settingsVersion,
      });
      if (result.success && result.project) {
        setProjectEnvelope((prev) =>
          prev ? { ...prev, project: result.project } : prev
        );
        setSettingsInfo('Settings saved.');
      } else {
        setSettingsError(result.error || 'Failed to save settings.');
      }
    } catch (error) {
      setSettingsError(toErrorMessage(error, 'Failed to save settings.'));
    } finally {
      setIsSavingSettings(false);
    }
  };

  const handleGenerateIcon = async () => {
    const trimmedPath = projectPath.trim();
    const trimmedPrompt = iconPrompt.trim();
    if (!trimmedPath || !trimmedPrompt) {
      setIconError('Enter an icon prompt.');
      return;
    }
    if (!selectedImageProviderHasKey) {
      setIconError(`Add your ${selectedImageProvider.keyLabel} in Settings before generating an icon.`);
      return;
    }

    setIsGeneratingIcon(true);
    setIconInfo(null);
    setIconError(null);
    setIconPreview(null);

    try {
      const result = await openCodeApi.generateIcon({
        path: trimmedPath,
        prompt: trimmedPrompt,
        imageSource,
        openaiKey: selectedImageProvider.id === 'openai'
          ? imageProviderKeys.openaiImageKey.trim() || undefined
          : undefined,
        geminiKey: selectedImageProvider.id === 'gemini'
          ? imageProviderKeys.geminiImageKey.trim() || undefined
          : undefined,
        xaiKey: selectedImageProvider.id === 'xai'
          ? imageProviderKeys.xaiImageKey.trim() || undefined
          : undefined,
        referenceImage: iconReferenceImage || undefined,
      });
      if (result.success) {
        setIconPreview(result.image || null);
        setIconInfo(`Icon generated${result.sourceService ? ` via ${result.sourceService}` : ''}.`);
      } else {
        setIconError(result.error || 'Failed to generate icon.');
      }
    } catch (error) {
      setIconError(toErrorMessage(error, 'Failed to generate icon.'));
    } finally {
      setIsGeneratingIcon(false);
    }
  };

  const openProjectInIDE = async (ide: OpenCodeIDE) => {
    const trimmedPath = projectPath.trim();
    if (!trimmedPath) {
      setIdeOpenError('Load a project first, then choose an IDE.');
      return;
    }

    setOpeningIDE(ide);
    setIdeOpenInfo(null);
    setIdeOpenError(null);

    try {
      const response = await openCodeApi.openProject({
        path: trimmedPath,
        ide,
      });
      if (!response.success) {
        throw new Error(response.error || 'Failed to open project.');
      }

      const action = ideActionFor(ideActions, ide);
      setIdeOpenInfo(`${action?.label || 'Open action'} triggered locally.`);
    } catch (error) {
      setIdeOpenError(toErrorMessage(error, 'Failed to open project locally.'));
    } finally {
      setOpeningIDE(null);
    }
  };

  const connectAuthProvider = async () => {
    setAuthConnectionInfo(null);
    setAuthConnectionError(null);

    if (authProvider !== 'chatgpt') {
      setAuthConnectionError('Selected auth provider is not supported yet.');
      return;
    }

    setIsUpdatingAuthConnection(true);
    let popupWindow: Window | null = null;
    try {
      popupWindow = window.open('', 'glowbom-chatgpt-oauth', 'popup,width=560,height=760');
      if (!popupWindow) {
        throw new Error('Popup blocked. Allow popups for this site and try Connect again.');
      }

      const startResponse = await openCodeApi.startOpenAIOAuth({
        projectPath: projectPath.trim() || undefined,
      });
      if (!startResponse.success || !startResponse.authorizationURL) {
        throw new Error(startResponse.error || 'Failed to start ChatGPT login flow.');
      }

      popupWindow.location.href = startResponse.authorizationURL;
      setAuthConnectionInfo('ChatGPT login opened in a popup. Complete login to finish connection.');

      const timeoutAt = Date.now() + 5 * 60 * 1000;
      while (Date.now() < timeoutAt) {
        const oauthStatus = await openCodeApi.getOpenAIOAuthStatus(startResponse.state);
        if (oauthStatus.phase === 'succeeded') {
          setAuthStatus(oauthStatus.status);
          setAuthConnectionInfo('ChatGPT account connected. You can build with your connected account now.');
          try {
            popupWindow.close();
          } catch {}
          return;
        }
        if (oauthStatus.phase === 'failed') {
          throw new Error(oauthStatus.error || 'ChatGPT login failed.');
        }

        await new Promise<void>((resolve) => {
          window.setTimeout(resolve, 1000);
        });
      }

      throw new Error('Timed out waiting for ChatGPT login completion. Try Connect again.');
    } catch (error) {
      setAuthConnectionError(toErrorMessage(error, 'Failed to connect ChatGPT account.'));
      if (popupWindow && !popupWindow.closed) {
        try {
          popupWindow.close();
        } catch {}
      }
    } finally {
      setIsUpdatingAuthConnection(false);
    }
  };

  const disconnectAuthProvider = async () => {
    setAuthConnectionInfo(null);
    setAuthConnectionError(null);

    setIsUpdatingAuthConnection(true);
    try {
      const response = await openCodeApi.disconnectOpenAIAuth({
        projectPath: projectPath.trim() || undefined,
      });
      if (!response.success) {
        throw new Error(response.error || 'Failed to disconnect auth provider.');
      }

      setAuthStatus(response.status);
      setAuthConnectionInfo('ChatGPT account disconnected for this local OpenCode runtime.');
    } catch (error) {
      setAuthConnectionError(toErrorMessage(error, 'Failed to disconnect ChatGPT account.'));
    } finally {
      setIsUpdatingAuthConnection(false);
    }
  };

  const restoreHistoryEntry = (entry: HistoryViewEntry) => {
    setInstructions(entry.instructions);
    setInstructionAttachments(entry.attachments);
    setInstructionPickerInfo(null);
    setInstructionPickerWarning(null);
    setFormError(null);
    setHistoryInfo(
      `Restored ${formatHistoryTimestamp(entry.timestamp)} with ${historyAttachmentCountLabel(entry.attachments.length)}.`,
    );
    if ((entry.missingAttachmentCount || 0) > 0) {
      setHistoryError(
        `${entry.missingAttachmentCount} archived file${entry.missingAttachmentCount === 1 ? ' is' : 's are'} missing, so only the available context was restored.`,
      );
    } else {
      setHistoryError(null);
    }
    setIsProjectPickerOpen(false);
  };

  const startRefine = async () => {
    setFormError(null);

    let hasLoadedProject = Boolean(activeProject);

    if (!hasLoadedProject) {
      if (!selectedProjectPath) {
        setFormError('Choose a local project folder first.');
        return;
      }

      const projectLoaded = await loadProject();
      if (!projectLoaded) {
        return;
      }
      hasLoadedProject = true;
    }

    if (!hasLoadedProject) {
      setFormError('Choose a valid Glowbom project folder before building.');
      return;
    }

    const finalInstructions = instructions.trim() || DEFAULT_BUILD_INSTRUCTIONS;

    let targetGuidance = '';
    const isAllSelected = selectedTargets.length === ALL_TARGET_IDS.length;

    if (!isAllSelected && selectedTargets.length > 0) {
      const selected = BUILD_TARGETS.filter((t) => selectedTargets.includes(t.id));
      const deselected = BUILD_TARGETS.filter((t) => !selectedTargets.includes(t.id));
      const selectedLabels = selected.map((t) => `${t.label} (${t.dir}/)`).join(', ');
      const deselectedDirs = deselected.map((t) => `${t.dir}/`).join(', ');

      if (selected.length === 1) {
        targetGuidance = `\n\nIMPORTANT: Please only work on ${selectedLabels}. Do not modify any other directories (${deselectedDirs}).`;
      } else {
        targetGuidance = `\n\nIMPORTANT: Please only work on ${selectedLabels}. Do not modify other directories (${deselectedDirs}).`;
      }
    }

    const finalInstructionsWithTargets = finalInstructions + targetGuidance;

    if (healthError || health?.healthy === false) {
      setFormError('Glowbom OSS cannot reach the local backend right now. Open Settings, refresh the checks, and try again.');
      setIsSettingsOpen(true);
      return;
    }

    const oversizedAttachment = instructionAttachments.find(
      (attachment) => attachment.sizeBytes > MAX_INSTRUCTION_ATTACHMENT_BYTES,
    );
    if (oversizedAttachment) {
      setFormError(
        `Attachment "${oversizedAttachment.name}" exceeds 40MB (${formatAttachmentSize(oversizedAttachment.sizeBytes)}). Remove it to continue.`,
      );
      return;
    }

    const instructionAttachmentPaths = instructionAttachments
      .map((attachment) => attachment.path.trim())
      .filter((value, index, values) => value.length > 0 && values.indexOf(value) === index);

    let modelValue = '';
    if (isOpenCodeConfigMode) {
      modelValue = resolvedOpenCodeModelValue;
    } else {
      const customModelTrimmed = customModel.trim();
      const customModelProvider = inferProviderFromCustomModel(customModelTrimmed);
      const providerForValidation = customModelProvider || selectedProvider;

      if (!providerForValidation && !customModelTrimmed) {
        setFormError('Select an AI model before building.');
        setIsSettingsOpen(true);
        return;
      }

      if (isAuthMode) {
        if (customModelProvider && customModelProvider !== 'openai') {
          setFormError('Auth provider mode currently supports OpenAI models only.');
          setIsSettingsOpen(true);
          return;
        }
        if (!isChatGPTConnected) {
          setFormError('Connect ChatGPT first to build with your connected account.');
          setIsSettingsOpen(true);
          return;
        }
      }

      if (isApiKeyMode && providerForValidation) {
        const providerDef = providerDefinition(providerForValidation);
        if (providerDef?.requiresKey && providerDef.keyField) {
          const requiredValue = providerKeys[providerDef.keyField].trim();
          if (!requiredValue) {
            if (providerDef.id === 'openai') {
              setFormError('OpenAI model selected. Add your OpenAI API key before building.');
            } else {
              setFormError(`${providerDef.label} model selected. Add ${providerDef.keyLabel || 'the provider API key'}.`);
            }
            setIsSettingsOpen(true);
            return;
          }
        }
      }

      modelValue = customModelTrimmed || resolvedSelection?.value || '';
      if (!modelValue) {
        setFormError('Model is required.');
        setIsSettingsOpen(true);
        return;
      }
    }

    setHistoryInfo(null);
    setHistoryError(null);
    setIsProjectPickerOpen(false);
    setIsContextOpen(false);
    setIsSettingsOpen(false);
    setOptimisticHistoryEntry({
      id: `optimistic-${Date.now()}`,
      timestamp: new Date().toISOString(),
      instructions: finalInstructions,
      taskType: 'refine',
      status: 'running',
      outputSummary: '',
      folderName: '',
      missingAttachmentCount: 0,
      attachments: instructionAttachments.map((attachment) => ({
        path: attachment.path,
        name: attachment.name,
        filename: attachment.name,
        sizeBytes: attachment.sizeBytes,
        mimeType: attachment.mimeType,
        mediaType: inferHistoryAttachmentMediaType(attachment.mimeType, attachment.name),
        relativePath: attachment.path,
      })),
      optimistic: true,
    });

    void refine.startRefine({
      projectPath: selectedProjectPath,
      instructions: finalInstructionsWithTargets,
      persistCurrentInstructionsToHistory: true,
      instructionAttachmentPaths,
      model: modelValue || undefined,
      openaiAuthMode: effectiveOpenAIAuthMode,
      providerKeys,
      imageProviderKeys: {
        openaiImageKey: selectedImageProvider.id === 'openai' ? imageProviderKeys.openaiImageKey : '',
        geminiImageKey: selectedImageProvider.id === 'gemini' ? imageProviderKeys.geminiImageKey : '',
        xaiImageKey: selectedImageProvider.id === 'xai' ? imageProviderKeys.xaiImageKey : '',
      },
      imageSource,
    });
  };

  const submitQuestion = async () => {
    const question = refine.pendingQuestion;
    if (!question) {
      return;
    }

    if (question.questions.length === 0) {
      const custom = simpleCustomAnswer.trim();
      const freeText = simpleAnswerText.trim();
      const selected = simpleSelectedOptions;

      const response = custom ? [custom] : selected.length > 0 ? selected : freeText ? [freeText] : [];
      const answer = response.join(', ');

      await refine.submitQuestion({
        answer,
        answers: response.length > 0 ? [response] : undefined,
      });
      return;
    }

    const answers: string[][] = [];
    const answerByQuestionID: Record<string, string[]> = {};
    const summaryParts: string[] = [];

    for (const item of question.questions) {
      const custom = (nestedCustomById[item.id] || '').trim();
      const selected = nestedSelectionsById[item.id] || [];
      const freeText = (nestedTextById[item.id] || '').trim();

      const response = custom ? [custom] : selected.length > 0 ? selected : freeText ? [freeText] : [];
      answers.push(response);

      if (response.length > 0) {
        answerByQuestionID[item.id] = response;
      }

      summaryParts.push(`Q: ${item.prompt}\nA: ${response.join(', ')}`);
    }

    await refine.submitQuestion({
      answer: summaryParts.join('\n\n').trim(),
      answers,
      answerByQuestionID: Object.keys(answerByQuestionID).length > 0 ? answerByQuestionID : undefined,
    });
  };

  const renderQuestionItem = (item: OpenCodeQuestionItem) => {
    const multi = isMultiSelect(item.prompt, item.inputType);
    const selected = nestedSelectionsById[item.id] || [];

    return (
      <div className="question-item" key={item.id}>
        <p className="question-item-title">{item.prompt}</p>

        {item.options.length > 0 ? (
          <div className="option-list">
            {item.options.map((option) => {
              const isSelected = selected.includes(option.label);

              return (
                <button
                  className={`option-button ${isSelected ? 'selected' : ''}`}
                  key={option.id || option.label}
                  onClick={() => {
                    setNestedSelectionsById((previous) => ({
                      ...previous,
                      [item.id]: toggleSelection(previous[item.id] || [], option.label, multi),
                    }));
                  }}
                  type="button"
                >
                  <span className="option-button-label">{option.label}</span>
                  {option.description ? <span className="option-button-description">{option.description}</span> : null}
                </button>
              );
            })}
          </div>
        ) : null}

        <textarea
          className="input"
          onChange={(event) => {
            setNestedTextById((previous) => ({
              ...previous,
              [item.id]: event.target.value,
            }));
          }}
          placeholder="Answer (optional)"
          rows={2}
          value={nestedTextById[item.id] || ''}
        />

        <input
          className="input"
          onChange={(event) => {
            setNestedCustomById((previous) => ({
              ...previous,
              [item.id]: event.target.value,
            }));
          }}
          placeholder="Custom response override (optional)"
          type="text"
          value={nestedCustomById[item.id] || ''}
        />
      </div>
    );
  };

  const renderConsoleLine = (line: string, index: number, isPartial: boolean) => {
    const normalized = line || ' ';
    const heading = parseConsoleHeading(normalized);
    const sharedClassName = `console-line${isPartial ? ' partial' : ''}`;

    if (heading) {
      return (
        <p className={`${sharedClassName} console-heading heading-${heading.level}`} key={`${index}-${line}`}>
          {heading.text}
        </p>
      );
    }

    if (lineLooksLikeMarkdown(normalized)) {
      const markdownHTML = parseConsoleMarkdown(normalized);
      if (markdownHTML) {
        return (
          <p
            className={`${sharedClassName} console-markdown`}
            dangerouslySetInnerHTML={{ __html: markdownHTML || '&nbsp;' }}
            key={`${index}-${line}`}
          />
        );
      }
    }

    return (
      <pre className={sharedClassName} key={`${index}-${line}`}>
        {normalized}
      </pre>
    );
  };

  const renderHistoryEntryCard = (entry: HistoryViewEntry) => {
    const attachments = entry.attachments || [];
    const missingAttachmentCount = entry.missingAttachmentCount || 0;
    const rawStatus = (entry.status || 'failed').toLowerCase();
    const statusKey = rawStatus in RUN_STATUS_LABEL ? rawStatus : 'idle';
    const statusLabel = RUN_STATUS_LABEL[statusKey] || 'Saved';

    const itemContent = (
      <>
        <div className="run-history-item-head">
          <div className="run-history-item-title">
            {entry.optimistic ? (
              <span aria-hidden="true" className="history-spinner" />
            ) : (
              <span>{formatHistoryTimestamp(entry.timestamp)}</span>
            )}
          </div>
          <span className={`run-status status-${statusKey}`}>{statusLabel}</span>
        </div>

        <p className="run-history-item-instructions">
          {compactHistoryText(
            entry.instructions,
            180,
            entry.attachments.length > 0 ? 'Saved files only.' : 'No saved instructions.',
          )}
        </p>

        {entry.outputSummary ? (
          <p className="run-history-item-summary">{compactHistoryText(entry.outputSummary, 220, '')}</p>
        ) : null}

        <div className="run-history-item-meta">
          <span>{historyAttachmentCountLabel(attachments.length)}</span>
          {missingAttachmentCount > 0 ? (
            <span>
              {missingAttachmentCount} missing file{missingAttachmentCount === 1 ? '' : 's'}
            </span>
          ) : null}
          {!entry.optimistic && entry.folderName ? <span>{entry.folderName}</span> : null}
        </div>

        {attachments.length > 0 ? (
          <div className="run-history-item-attachments">
            {attachments.slice(0, 3).map((attachment) => (
              <span className="history-attachment-chip" key={`${entry.id}-${attachment.path}`}>
                {attachment.name}
              </span>
            ))}
            {attachments.length > 3 ? (
              <span className="history-attachment-chip">+{attachments.length - 3} more</span>
            ) : null}
          </div>
        ) : null}
      </>
    );

    if (entry.optimistic) {
      return (
        <div className="run-history-item optimistic" key={entry.id}>
          {itemContent}
        </div>
      );
    }

    return (
      <button
        className="run-history-item"
        key={entry.id || `${entry.folderName}-${entry.timestamp}`}
        onClick={() => restoreHistoryEntry(entry)}
        type="button"
      >
        {itemContent}
      </button>
    );
  };

  const renderProjectLaunchButton = (
    ide: OpenCodeIDE,
    label: string,
    action: OpenCodeProjectIDEAction | undefined,
  ) => {
    const isOpening = openingIDE === ide;
    const isDisabled = !action?.available || openingIDE !== null;
    const title = action?.available ? `Open in ${label}` : `${label} unavailable`;

    return (
      <button
        aria-label={isOpening ? `Opening in ${label}` : title}
        className={`project-launch-button ${isOpening ? 'opening' : ''}`}
        disabled={isDisabled}
        key={ide}
        onClick={() => {
          void openProjectInIDE(ide);
        }}
        title={title}
        type="button"
      >
        {isOpening ? <span aria-hidden="true" className="history-spinner toolbar-spinner" /> : <IDEQuickActionIcon ide={ide} />}
        <span className="sr-only">{label}</span>
      </button>
    );
  };

  return (
    <div className="app-shell">
      <header className="topbar">
        <div className="topbar-inner">
          <a className="topbar-logo" href="https://glowbom.com" rel="noreferrer" target="_blank">
            <img alt="Glowbom" src={topbarLogoSrc} />
          </a>
          <nav className="topbar-links">
            <a href="https://glowbom.com/oss" rel="noreferrer" target="_blank">
              Glowbom OSS
            </a>
            <a href="https://glowbom.com/desktop/" rel="noreferrer" target="_blank">
              Desktop
            </a>
            <a href="https://glowbom.com/terms.html" rel="noreferrer" target="_blank">
              Terms
            </a>
            <a href="https://glowbom.com/pricing/" rel="noreferrer" target="_blank">
              Pricing
            </a>
            <a href="https://glowbom.com/docs/" rel="noreferrer" target="_blank">
              Docs
            </a>
            <a href="https://glowbom.com/#case_studies" rel="noreferrer" target="_blank">
              Apps
            </a>
          </nav>
          <a className="button topbar-cta" href="https://glowbom.com/draw" rel="noreferrer" target="_blank">
            Get Started for Free
          </a>
        </div>
      </header>

      <main className="page">
        <header className="brand-header brand-header-minimal">
          <div className="brand-copy">
            <h1>Glowbom OSS</h1>
            <p>Build Anything Locally</p>
          </div>
        </header>

        <section className="card composer-card">
          <div className="composer-toolbar">
            <div className="composer-toolbar-primary">
              <button
                className={`composer-chip-button ${activeProject ? 'has-value' : ''} ${isProjectPickerOpen ? 'active' : ''}`}
                disabled={refine.isRunning || isLoadingProject}
                onClick={toggleProjectPicker}
                type="button"
              >
                <span className="composer-chip-label">Project</span>
                <span className="composer-chip-value">{projectButtonLabel}</span>
              </button>

              {activeProject ? (
                <div className="project-launch-strip" role="group" aria-label="Open project in apps">
                  {renderProjectLaunchButton('finder', 'Finder', finderAction)}
                  {renderProjectLaunchButton('xcode', 'Xcode', xcodeAction)}
                  {renderProjectLaunchButton('android-studio', 'Android Studio', androidStudioAction)}
                  {renderProjectLaunchButton('vscode', 'VS Code', vscodeAction)}
                </div>
              ) : null}
            </div>

            <div className="row composer-toolbar-actions">
              <button
                className={`composer-chip-button ${attachmentCount > 0 ? 'has-value' : ''} ${isContextOpen ? 'active' : ''}`}
                disabled={refine.isRunning}
                onClick={toggleContextPanel}
                type="button"
              >
                <span className="composer-chip-value">{contextButtonLabel}</span>
              </button>
              <button
                className={`composer-chip-button ${systemNeedsAttention ? 'attention' : ''} ${isSettingsOpen ? 'active' : ''}`}
                disabled={refine.isRunning}
                onClick={toggleSettingsPanel}
                type="button"
              >
                <span className="composer-chip-value">Settings</span>
              </button>
            </div>
          </div>

          {isProjectPickerOpen ? (
            <div className="composer-popover">
              <div className="composer-popover-header">
                <strong>Project</strong>
                {activeProject || selectedProjectPath ? (
                  <button className="button secondary tiny" onClick={clearProjectSelection} type="button">
                    Clear
                  </button>
                ) : null}
              </div>

              <div className="row compact">
                <input
                  className="input"
                  id="projectPath"
                  onChange={(event) => {
                    setProjectPath(event.target.value);
                    setFormError(null);
                    setProjectEnvelope(null);
                    setProjectError(null);
                    setIdeStatus(null);
                    setIdeStatusError(null);
                    setIdeOpenInfo(null);
                    setIdeOpenError(null);
                  }}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter' && !isLoadingProject && !refine.isRunning) {
                      event.preventDefault();
                      void loadProject(event.currentTarget.value);
                    }
                  }}
                  placeholder="/absolute/path/to/project"
                  value={projectPath}
                />
                <button
                  className="button secondary"
                  disabled={refine.isRunning || isPickingFolder || isLoadingProject}
                  onClick={() => void openFolderPicker()}
                  type="button"
                >
                  {isPickingFolder ? 'Choosing...' : isLoadingProject ? 'Loading...' : 'Choose'}
                </button>
              </div>

              {folderPickerInfo ? <p className="meta ok">{folderPickerInfo}</p> : null}
              {folderPickerWarning ? <p className="meta warn">{folderPickerWarning}</p> : null}
              {projectError ? <p className="error-inline">{projectError}</p> : null}

              {activeProject ? (
                <div className="project-summary compact-project-summary">
                  <div className="project-summary-header">
                    <div className="project-heading">
                      {isRenamingProject ? (
                        <div className="project-rename-row">
                          <input
                            className="project-rename-input"
                            type="text"
                            value={renameValue}
                            onChange={(e) => setRenameValue(e.target.value)}
                            onKeyDown={(e) => {
                              if (e.key === 'Enter') void handleRenameProject(renameValue);
                              if (e.key === 'Escape') { setIsRenamingProject(false); setRenameError(null); }
                            }}
                            autoFocus
                          />
                          <button
                            className="button secondary tiny"
                            onClick={() => void handleRenameProject(renameValue)}
                            type="button"
                          >
                            Save
                          </button>
                          <button
                            className="button secondary tiny"
                            onClick={() => { setIsRenamingProject(false); setRenameError(null); }}
                            type="button"
                          >
                            Cancel
                          </button>
                        </div>
                      ) : (
                        <div className="project-name-row">
                          <strong>{activeProject.name}</strong>
                          <button
                            className="button secondary tiny"
                            onClick={() => { setRenameValue(activeProject.name); setIsRenamingProject(true); setRenameError(null); }}
                            type="button"
                            title="Rename project"
                          >
                            Rename
                          </button>
                        </div>
                      )}
                      {renameError ? <p className="error-inline">{renameError}</p> : null}
                      <span className="meta">
                        v{activeProject.version} · {targetRows.length} target{targetRows.length === 1 ? '' : 's'}
                      </span>
                    </div>
                  </div>
                  <p className="meta project-path-meta">{selectedProjectPath}</p>
                  <div className="chip-list">
                    {BUILD_TARGETS.map((target) => (
                      <label className={`chip chip-selectable ${selectedTargets.includes(target.id) ? '' : 'chip-deselected'}`} key={target.id}>
                        <input
                          type="checkbox"
                          checked={selectedTargets.includes(target.id)}
                          onChange={() => toggleTarget(target.id)}
                          disabled={refine.isRunning}
                        />
                        {target.label}
                      </label>
                    ))}
                  </div>

                  <details className="project-tools-disclosure">
                    <summary>Tools</summary>
                    <div className="project-ide-tools">
                      <div className="card-title-row">
                        <strong>Open</strong>
                        <button
                          className="button secondary tiny"
                          disabled={isLoadingIdeStatus}
                          onClick={() => void refreshIDEStatus(projectPath)}
                          type="button"
                        >
                          {isLoadingIdeStatus ? 'Scanning...' : 'Refresh'}
                        </button>
                      </div>

                      <div className="row">
                        <button
                          className="button secondary"
                          disabled={!finderAction?.available || openingIDE !== null}
                          onClick={() => {
                            void openProjectInIDE('finder');
                          }}
                          type="button"
                        >
                          {openingIDE === 'finder' ? 'Opening...' : 'Folder'}
                        </button>
                        <button
                          className="button secondary"
                          disabled={!xcodeAction?.available || openingIDE !== null}
                          onClick={() => {
                            void openProjectInIDE('xcode');
                          }}
                          type="button"
                        >
                          {openingIDE === 'xcode' ? 'Opening...' : 'Xcode'}
                        </button>
                        <button
                          className="button secondary"
                          disabled={!androidStudioAction?.available || openingIDE !== null}
                          onClick={() => {
                            void openProjectInIDE('android-studio');
                          }}
                          type="button"
                        >
                          {openingIDE === 'android-studio' ? 'Opening...' : 'Android Studio'}
                        </button>
                        <button
                          className="button secondary"
                          disabled={!vscodeAction?.available || openingIDE !== null}
                          onClick={() => {
                            void openProjectInIDE('vscode');
                          }}
                          type="button"
                        >
                          {openingIDE === 'vscode' ? 'Opening...' : 'VS Code'}
                        </button>
                      </div>

                      {ideStatusError ? <p className="error-inline">{ideStatusError}</p> : null}
                      {ideOpenInfo ? <p className="meta ok">{ideOpenInfo}</p> : null}
                      {ideOpenError ? <p className="error-inline">{ideOpenError}</p> : null}
                    </div>
                  </details>

                  <details className="project-tools-disclosure">
                    <summary>Project Settings</summary>
                    <div className="project-settings-grid">
                      <label className="field-label-inline">
                        <span>Display Name</span>
                        <input
                          className="input"
                          disabled={refine.isRunning || isSavingSettings}
                          onChange={(e) => setSettingsDisplayName(e.target.value)}
                          placeholder="My App"
                          value={settingsDisplayName}
                        />
                      </label>
                      <label className="field-label-inline">
                        <span>Bundle ID</span>
                        <input
                          className="input"
                          disabled={refine.isRunning || isSavingSettings}
                          onChange={(e) => setSettingsBundleID(e.target.value)}
                          placeholder="app.glowbom.myapp"
                          value={settingsBundleID}
                        />
                      </label>
                      <label className="field-label-inline">
                        <span>Version</span>
                        <input
                          className="input"
                          disabled={refine.isRunning || isSavingSettings}
                          onChange={(e) => setSettingsVersion(e.target.value)}
                          placeholder="1.0"
                          value={settingsVersion}
                        />
                      </label>
                      <label className="field-label-inline">
                        <span>Build Number</span>
                        <input
                          className="input"
                          disabled={refine.isRunning || isSavingSettings}
                          onChange={(e) => setSettingsBuildNumber(e.target.value)}
                          placeholder="1"
                          value={settingsBuildNumber}
                        />
                      </label>
                      <div className="row">
                        <button
                          className="button"
                          disabled={refine.isRunning || isSavingSettings}
                          onClick={() => void handleSaveProjectSettings()}
                          type="button"
                        >
                          {isSavingSettings ? 'Saving...' : 'Save Settings'}
                        </button>
                      </div>
                      {settingsInfo ? <p className="meta ok">{settingsInfo}</p> : null}
                      {settingsError ? <p className="error-inline">{settingsError}</p> : null}

                      <div className="icon-section">
                        <strong>App Icon</strong>
                        <div className="icon-row">
                          {iconPreview ? (
                            <img className="icon-preview" src={iconPreview} alt="App icon" />
                          ) : (
                            <div className="icon-preview icon-placeholder">No icon</div>
                          )}
                          {iconReferenceImage ? (
                            <div className="icon-ref-wrapper">
                              <img className="icon-ref-preview" src={iconReferenceImage} alt="Reference" />
                              <button
                                className="icon-ref-clear"
                                onClick={() => setIconReferenceImage(null)}
                                type="button"
                                title="Remove reference image"
                              >
                                &times;
                              </button>
                              <span className="meta">Reference</span>
                            </div>
                          ) : null}
                        </div>
                        {isGeneratingIcon ? (
                          <div className="icon-progress">
                            <div className="icon-progress-bar" />
                            <span className="meta">Generating icon...</span>
                          </div>
                        ) : null}
                        <input
                          className="input"
                          disabled={refine.isRunning || isGeneratingIcon}
                          onChange={(e) => setIconPrompt(e.target.value)}
                          onKeyDown={(e) => {
                            if (e.key === 'Enter') void handleGenerateIcon();
                          }}
                          placeholder="Describe your app icon..."
                          value={iconPrompt}
                        />
                        <div className="row">
                          <button
                            className="button"
                            disabled={refine.isRunning || isGeneratingIcon || !iconPrompt.trim()}
                            onClick={() => void handleGenerateIcon()}
                            type="button"
                          >
                            {isGeneratingIcon ? 'Generating...' : 'Generate Icon'}
                          </button>
                          <label className="button secondary" style={{ cursor: refine.isRunning || isGeneratingIcon ? 'not-allowed' : 'pointer' }}>
                            {iconReferenceImage ? 'Change Ref' : 'Add Reference'}
                            <input
                              type="file"
                              accept="image/*"
                              style={{ display: 'none' }}
                              disabled={refine.isRunning || isGeneratingIcon}
                              onChange={(e) => {
                                const file = e.target.files?.[0];
                                if (!file) return;
                                const reader = new FileReader();
                                reader.onload = () => {
                                  setIconReferenceImage(reader.result as string);
                                };
                                reader.readAsDataURL(file);
                                e.target.value = '';
                              }}
                            />
                          </label>
                        </div>
                        {iconInfo ? <p className="meta ok">{iconInfo}</p> : null}
                        {iconError ? <p className="error-inline">{iconError}</p> : null}
                      </div>
                    </div>
                  </details>

                  <div className="project-history-section">
                    <div className="card-title-row">
                      <strong>History</strong>
                      <button
                        className="button secondary tiny"
                        disabled={isLoadingHistory || !selectedProjectPath || !activeProject}
                        onClick={() => {
                          void refreshProjectHistory(selectedProjectPath);
                        }}
                        type="button"
                      >
                        {isLoadingHistory ? 'Refreshing...' : 'Refresh'}
                      </button>
                    </div>

                    {historyError ? <p className="error-inline">{historyError}</p> : null}

                    {historyEntries.length === 0 ? (
                      <div className="empty-inline-state compact-empty-state">
                        <strong>{isLoadingHistory ? 'Loading history...' : 'No history yet'}</strong>
                        <p className="meta">Build once and Glowbom OSS will archive the prompt and attached files here.</p>
                      </div>
                    ) : (
                      <div className="run-history-list project-history-list">
                        {historyEntries.map((entry) => renderHistoryEntryCard(entry))}
                      </div>
                    )}
                  </div>
                </div>
              ) : null}

              {projectHistory.length > 0 ? (
                <div className="history-list">
                  {projectHistory.map((entry) => {
                    const isCurrent = entry.path === selectedProjectPath;
                    return (
                      <button
                        className={`history-item ${isCurrent ? 'current' : ''}`}
                        key={entry.path}
                        onClick={() => {
                          setProjectPath(entry.path);
                          void loadProject(entry.path);
                        }}
                        type="button"
                      >
                        <span className="history-item-title">{entry.name}</span>
                        <span className="history-item-meta">{entry.path}</span>
                      </button>
                    );
                  })}
                </div>
              ) : null}
            </div>
          ) : null}

          {isContextOpen ? (
            <div className="composer-popover">
              <div className="composer-popover-header">
                <strong>Context</strong>
                <div className="row">
                  <button
                    className="button secondary tiny"
                    disabled={refine.isRunning || isPickingInstructionFiles}
                    onClick={() => {
                      void pickInstructionFiles();
                    }}
                    type="button"
                  >
                    {isPickingInstructionFiles ? 'Choosing...' : 'Add files'}
                  </button>
                  <button
                    className="button secondary tiny"
                    disabled={refine.isRunning || instructionAttachments.length === 0}
                    onClick={() => {
                      setInstructionAttachments([]);
                      setInstructionPickerInfo(null);
                      setInstructionPickerWarning(null);
                    }}
                    type="button"
                  >
                    Clear
                  </button>
                </div>
              </div>

              {instructionPickerInfo ? <p className="meta ok">{instructionPickerInfo}</p> : null}
              {instructionPickerWarning ? <p className="meta warn">{instructionPickerWarning}</p> : null}

              {instructionAttachments.length > 0 ? (
                <ul className="attachment-list">
                  {instructionAttachments.map((attachment) => {
                    const oversize = attachment.sizeBytes > MAX_INSTRUCTION_ATTACHMENT_BYTES;
                    return (
                      <li className={`attachment-item ${oversize ? 'oversize' : ''}`} key={attachment.path}>
                        <div className="attachment-item-copy">
                          <span className="attachment-name">{attachment.name}</span>
                          <span className="attachment-meta">
                            {formatAttachmentSize(attachment.sizeBytes)}
                            {attachment.mimeType ? ` · ${attachment.mimeType}` : ''}
                          </span>
                        </div>
                        <button
                          className="button secondary tiny"
                          disabled={refine.isRunning}
                          onClick={() => {
                            setInstructionAttachments((previous) => previous.filter((item) => item.path !== attachment.path));
                          }}
                          type="button"
                        >
                          Remove
                        </button>
                      </li>
                    );
                  })}
                </ul>
              ) : (
                <p className="meta">No files.</p>
              )}
            </div>
          ) : null}

          {isSettingsOpen ? (
            <div className="composer-popover settings-popover">
              <div className="composer-popover-header">
                <strong>Settings</strong>
                <button
                  className="button secondary tiny"
                  disabled={isCheckingSetup || refine.isRunning}
                  onClick={() => void refreshSetup()}
                  type="button"
                >
                  {isCheckingSetup ? 'Checking...' : 'Refresh'}
                </button>
              </div>

              <div className="summary-pill-row">
                <span
                  className={`summary-pill ${
                    systemNeedsAttention ? 'tone-warning' : health?.healthy ? 'tone-success' : 'tone-neutral'
                  }`}
                >
                  {systemStatusSummary}
                </span>
                <span className={`summary-pill ${isAuthMode && !authProviderConnected ? 'tone-warning' : 'tone-neutral'}`}>
                  {agentStatusSummary}
                </span>
                <span className={`summary-pill ${selectedImageProviderHasKey ? 'tone-success' : 'tone-warning'}`}>
                  Images: {imageProviderStatusSummary}
                </span>
              </div>

              <div className="field-grid">
                <div>
                  <label className="field-label" htmlFor="credentialMode">
                    AI access
                  </label>
                  <select
                    className="input"
                    disabled={refine.isRunning}
                    id="credentialMode"
                    onChange={(event) => setCredentialMode(event.target.value as CredentialMode)}
                    value={credentialMode}
                  >
                    {/* Connected accounts and direct API keys return with the shared desktop onboarding. */}
                    <option value="opencode-config">OpenCode setup</option>
                  </select>
                </div>

                {isAuthMode ? (
                  <div>
                    <label className="field-label" htmlFor="authProvider">
                      Account
                    </label>
                    <select
                      className="input"
                      disabled={refine.isRunning || isUpdatingAuthConnection}
                      id="authProvider"
                      onChange={(event) => {
                        setAuthProvider(event.target.value as AuthProviderID);
                        setAuthConnectionInfo(null);
                        setAuthConnectionError(null);
                      }}
                      value={authProvider}
                    >
                      <option value="chatgpt">ChatGPT</option>
                    </select>
                  </div>
                ) : null}
              </div>

              <div className="field-grid">
                <div>
                  <label className="field-label" htmlFor="modelPreset">
                    Model
                  </label>
                  {isOpenCodeConfigMode ? (
                    <select
                      className="input"
                      disabled={refine.isRunning || isLoadingOpenCodeModels}
                      id="modelPreset"
                      onChange={(event) => setSelectedOpenCodeModel(event.target.value)}
                      value={selectedOpenCodeModel}
                    >
                      <option value={OPENCODE_RECOMMENDED_MODEL_VALUE}>
                        {recommendedOpenCodeModelOptionLabel}
                      </option>
                      {recommendedOpenCodeModel ? (
                        <option value={OPENCODE_DEFAULT_MODEL_VALUE}>OpenCode default</option>
                      ) : null}
                      {openCodeConfigProviders.map((provider) => (
                        <optgroup key={provider.id} label={provider.displayName || provider.id}>
                          {provider.models.map((model) => (
                            <option key={`${provider.id}-${model.id}`} value={`${provider.id}/${model.id}`}>
                              {model.displayName || model.id}
                            </option>
                          ))}
                        </optgroup>
                      ))}
                    </select>
                  ) : (
                    <select
                      className="input"
                      disabled={refine.isRunning || allOptionValues.length === 0}
                      id="modelPreset"
                      onChange={(event) => setSelectedModelOption(event.target.value)}
                      value={selectedModelOption}
                    >
                      {visibleModelGroups.map((group) => (
                        <optgroup key={group.provider.id} label={group.provider.label}>
                          {group.models.map((model) => {
                            const value = encodeModelOptionValue(group.provider.id, model.id);
                            return (
                              <option key={`${group.provider.id}-${model.id}`} value={value}>
                                {model.label}
                              </option>
                            );
                          })}
                        </optgroup>
                      ))}
                    </select>
                  )}
                </div>

                {!isOpenCodeConfigMode ? (
                  <div>
                    <label className="field-label" htmlFor="customModel">
                      Custom
                    </label>
                    <input
                      className="input"
                      disabled={refine.isRunning}
                      id="customModel"
                      onChange={(event) => setCustomModel(event.target.value)}
                      placeholder="provider/model"
                      value={customModel}
                    />
                  </div>
                ) : null}
              </div>

              <section className="image-generation-settings" aria-labelledby="imageGenerationHeading">
                <div className="settings-section-heading">
                  <strong id="imageGenerationHeading">Image Generation</strong>
                  <span className="meta">Used for app icons and project images. Separate from your coding agent.</span>
                </div>

                <div>
                  <label className="field-label" htmlFor="imageSource">
                    Provider
                  </label>
                  <select
                    className="input"
                    disabled={refine.isRunning}
                    id="imageSource"
                    onChange={(event) => setImageSource(event.target.value)}
                    value={imageSource}
                  >
                    {IMAGE_PROVIDERS.map((provider) => (
                      <option key={provider.id} value={provider.source}>
                        {provider.label}
                      </option>
                    ))}
                  </select>
                </div>

                <div className="field-grid image-provider-keys-grid">
                  {IMAGE_PROVIDERS.map((provider) => {
                    const isActive = provider.id === selectedImageProvider.id;
                    return (
                      <label className={`provider-key-item ${isActive ? 'active' : ''}`} key={provider.id}>
                        <span className="field-label">{provider.keyLabel}</span>
                        <input
                          className="input"
                          disabled={refine.isRunning}
                          onChange={(event) => updateImageProviderKey(provider.keyField, event.target.value)}
                          placeholder={provider.keyPlaceholder}
                          type="password"
                          value={imageProviderKeys[provider.keyField]}
                        />
                      </label>
                    );
                  })}
                </div>
                <span className="meta">Keys stay in this browser and are sent only to the local backend when used.</span>
              </section>

              {isAuthMode ? (
                <div className={`auth-connection-card ${authProviderConnected ? 'connected' : 'disconnected'}`}>
                  <strong>{authProviderConnected ? 'ChatGPT connected' : 'ChatGPT not connected'}</strong>
                  <span className="meta">Credential: {formatCredentialType(authStatus?.openaiCredentialType)}</span>
                  <div className="row">
                    {authProviderConnected ? (
                      <button
                        className="button secondary"
                        disabled={refine.isRunning || isUpdatingAuthConnection}
                        onClick={() => {
                          void disconnectAuthProvider();
                        }}
                        type="button"
                      >
                        {isUpdatingAuthConnection ? 'Disconnecting...' : 'Disconnect'}
                      </button>
                    ) : (
                      <button
                        className="button"
                        disabled={refine.isRunning || isUpdatingAuthConnection}
                        onClick={() => {
                          void connectAuthProvider();
                        }}
                        type="button"
                      >
                        {isUpdatingAuthConnection ? 'Connecting...' : 'Connect'}
                      </button>
                    )}
                  </div>
                  {authConnectionInfo ? <p className="meta ok">{authConnectionInfo}</p> : null}
                  {authConnectionError ? <p className="error-inline">{authConnectionError}</p> : null}
                </div>
              ) : null}

              {isApiKeyMode ? (
                <div className="field-grid">
                  <div>
                    <label className="field-label" htmlFor="openaiKey">
                      OpenAI key
                    </label>
                    <input
                      className="input"
                      disabled={refine.isRunning}
                      id="openaiKey"
                      onChange={(event) => updateProviderKey('openaiKey', event.target.value)}
                      placeholder="sk-..."
                      type="password"
                      value={providerKeys.openaiKey}
                    />
                  </div>
                </div>
              ) : null}

              {isApiKeyMode ? (
                <details className="provider-keys">
                  <summary>Other keys</summary>
                  <div className="field-grid provider-keys-grid">
                    {MODEL_PROVIDERS.filter((provider) => provider.keyField && provider.id !== 'openai').map((provider) => {
                      const keyField = provider.keyField!;
                      const isActive = provider.id === selectedProvider;

                      return (
                        <label className={`provider-key-item ${isActive ? 'active' : ''}`} key={provider.id}>
                          <span className="field-label">{provider.keyLabel || provider.label}</span>
                          <input
                            className="input"
                            disabled={refine.isRunning}
                            onChange={(event) => updateProviderKey(keyField, event.target.value)}
                            placeholder={`${provider.label} key`}
                            type="password"
                            value={providerKeys[keyField]}
                          />
                        </label>
                      );
                    })}
                    <label className="provider-key-item">
                      <span className="field-label">ElevenLabs API Key</span>
                      <input
                        className="input"
                        disabled={refine.isRunning}
                        onChange={(event) => updateProviderKey('elevenLabsKey', event.target.value)}
                        placeholder="ElevenLabs key"
                        type="password"
                        value={providerKeys.elevenLabsKey}
                      />
                    </label>
                  </div>
                </details>
              ) : null}

              <div className="status-grid">
                <div className="status-tile">
                  <span className="status-label">Backend</span>
                  <strong className={health?.healthy ? 'ok' : health ? 'warn' : ''}>{setupStatusText}</strong>
                  {health?.server ? <span className="meta">{health.server}</span> : null}
                  {health?.hint ? <span className="meta">{health.hint}</span> : null}
                  {healthError ? <span className="error-inline">{healthError}</span> : null}
                </div>

                <div className="status-tile">
                  <span className="status-label">Agent server</span>
                  <strong className={authStatus?.serverRunning ? 'ok' : authStatus ? 'warn' : ''}>
                    {authStatus ? (authStatus.serverRunning ? 'Running' : 'Stopped') : 'Checking...'}
                  </strong>
                  <span className="meta">Auth: {formatAuthMode(authStatus?.cachedGlowbomAuthMode)}</span>
                  {authError ? <span className="error-inline">{authError}</span> : null}
                </div>
              </div>

              {isLoadingOpenAIModels ? <p className="meta">Loading OpenAI models...</p> : null}
              {openAIModelsInfo ? <p className="meta ok">{openAIModelsInfo}</p> : null}
              {openAIModelsWarning ? <p className="meta warn">{openAIModelsWarning}</p> : null}
              {isLoadingOpenCodeModels ? <p className="meta">Loading OpenCode models...</p> : null}
              {openCodeModelsInfo ? <p className="meta ok">{openCodeModelsInfo}</p> : null}
              {openCodeModelsWarning ? <p className="meta warn">{openCodeModelsWarning}</p> : null}
            </div>
          ) : null}

          <div className="composer-input-wrap">
            {refine.isRunning ? (
              optimisticHistoryEntry ? (
                renderHistoryEntryCard(optimisticHistoryEntry)
              ) : (
                <div className="empty-inline-state">
                  <strong>Preparing current build...</strong>
                  <p className="meta">Glowbom OSS is working on the current run.</p>
                </div>
              )
            ) : (
              <textarea
                className="input minimal-input"
                disabled={refine.isRunning}
                id="instructions"
                onChange={(event) => {
                  setInstructions(event.target.value);
                  setFormError(null);
                }}
                placeholder="What should Glowbom build?"
                rows={7}
                value={instructions}
              />
            )}
          </div>

          {formError ? <p className="error-inline composer-error">{formError}</p> : null}
          {!formError && projectError && !isProjectPickerOpen ? <p className="error-inline composer-error">{projectError}</p> : null}
          {!isProjectPickerOpen && historyError ? <p className="error-inline composer-error">{historyError}</p> : null}
          {!isProjectPickerOpen && historyInfo ? <p className="meta ok composer-note">{historyInfo}</p> : null}

          <div className="composer-footer minimal-composer-footer">
            <div className="composer-footer-copy">
              {refine.summary ? <p className="summary-text">{refine.summary}</p> : null}
              {refine.hasSession && !refine.isRunning ? (
                <label className="toggle-label">
                  <input
                    type="checkbox"
                    checked={refine.continueSession}
                    onChange={(event) => refine.setContinueSession(event.target.checked)}
                  />
                  Continue last session
                </label>
              ) : !refine.summary ? (
                <p className="meta minimal-meta">
                  {activeProject ? projectButtonLabel : 'Choose a project'}
                  {` · `}
                  {agentStatusSummary}
                </p>
              ) : null}
            </div>
            <div className="row composer-actions">
              <span className={`run-status status-${refine.status}`}>{RUN_STATUS_LABEL[refine.status]}</span>
              <button
                className="button"
                disabled={refine.isRunning || refine.isSubmittingInput || isLoadingProject}
                onClick={() => {
                  void startRefine();
                }}
                type="button"
              >
                Build
              </button>
              <button className="button danger" disabled={!refine.isRunning} onClick={refine.stopRun} type="button">
                Stop
              </button>
            </div>
          </div>
        </section>

        <section className="card activity-card">
          <div className="card-title-row">
            <div>
              <h2>Activity</h2>
              <p className="meta">Logs, follow-up questions, permissions, and media approvals show up here while Glowbom OSS works.</p>
            </div>
            <label className="checkbox-row">
              <input
                checked={autoScrollEnabled}
                onChange={(event) => setAutoScrollEnabled(event.target.checked)}
                type="checkbox"
              />
              Auto-scroll
            </label>
          </div>

          {refine.error ? <p className="error-inline">{refine.error}</p> : null}

          <div className="console" onScroll={onConsoleScroll} ref={consoleRef}>
            {runLogs.length === 0 ? (
              <p className="placeholder">Ready. Choose a project folder, describe the work, and click Build.</p>
            ) : (
              runLogs.map((line, index) =>
                renderConsoleLine(line, index, index === runLogs.length - 1 && !!refine.partialLine),
              )
            )}
          </div>

          {refine.changedFiles.length > 0 ? (
            <details className="changed-files">
              <summary>Changed files ({refine.changedFiles.length})</summary>
              <ul>
                {refine.changedFiles.slice(0, 200).map((filePath) => (
                  <li key={filePath}>{filePath}</li>
                ))}
              </ul>
            </details>
          ) : null}

          {refine.pendingQuestion ? (
          <div className="input-panel">
            <h3>Agent needs input</h3>
            <p>{refine.pendingQuestion.prompt}</p>

            {refine.pendingQuestion.questions.length > 0 ? (
              <div className="question-list">{refine.pendingQuestion.questions.map(renderQuestionItem)}</div>
            ) : (
              <div className="question-item">
                {refine.pendingQuestion.options.length > 0 ? (
                  <div className="option-list">
                    {refine.pendingQuestion.options.map((option) => {
                      const multi = isMultiSelect(refine.pendingQuestion?.prompt || '');
                      const selected = simpleSelectedOptions.includes(option.label);

                      return (
                        <button
                          className={`option-button ${selected ? 'selected' : ''}`}
                          key={option.id || option.label}
                          onClick={() => {
                            setSimpleSelectedOptions((previous) =>
                              toggleSelection(previous, option.label, multi),
                            );
                          }}
                          type="button"
                        >
                          <span className="option-button-label">{option.label}</span>
                          {option.description ? (
                            <span className="option-button-description">{option.description}</span>
                          ) : null}
                        </button>
                      );
                    })}
                  </div>
                ) : null}

                <textarea
                  className="input"
                  onChange={(event) => setSimpleAnswerText(event.target.value)}
                  placeholder="Your answer"
                  rows={3}
                  value={simpleAnswerText}
                />

                <input
                  className="input"
                  onChange={(event) => setSimpleCustomAnswer(event.target.value)}
                  placeholder="Custom response override (optional)"
                  type="text"
                  value={simpleCustomAnswer}
                />
              </div>
            )}

            <div className="row">
              <button
                className="button"
                disabled={refine.isSubmittingInput}
                onClick={() => {
                  void submitQuestion();
                }}
              >
                {refine.isSubmittingInput ? 'Sending...' : 'Send'}
              </button>
              <button
                className="button secondary"
                disabled={refine.isSubmittingInput}
                onClick={() => {
                  void refine.submitQuestion({ answer: '' });
                }}
              >
                Dismiss
              </button>
            </div>
          </div>
        ) : null}

          {refine.pendingPermission ? (
          <div className="input-panel">
            <h3>Permission requested</h3>
            <p>{refine.pendingPermission.title}</p>
            {refine.pendingPermission.message ? <p className="meta-text">{refine.pendingPermission.message}</p> : null}
            {refine.pendingPermission.pattern ? (
              <p className="meta-text">Pattern: {refine.pendingPermission.pattern}</p>
            ) : null}

            <div className="row">
              <button
                className="button"
                disabled={refine.isSubmittingInput}
                onClick={() => {
                  void refine.respondToPermission('once');
                }}
              >
                Allow once
              </button>
              <button
                className="button secondary"
                disabled={refine.isSubmittingInput}
                onClick={() => {
                  void refine.respondToPermission('always');
                }}
              >
                Always allow
              </button>
              <button
                className="button danger"
                disabled={refine.isSubmittingInput}
                onClick={() => {
                  void refine.respondToPermission('reject');
                }}
              >
                Deny
              </button>
            </div>
          </div>
          ) : null}

          {refine.pendingMediaApproval ? (
            <div className="input-panel">
              <h3>{refine.pendingMediaApproval.title}</h3>
              {refine.pendingMediaApproval.message ? (
                <p className="meta-text">{refine.pendingMediaApproval.message}</p>
              ) : null}

              <div className="media-approval-list">
                {refine.pendingMediaApproval.items.map((item, index) => {
                  const typeLabel = item.audioType || item.mediaType;
                  return (
                    <div className="media-approval-item" key={`${item.mediaType}-${item.prompt}-${index}`}>
                      <div className="media-approval-item-heading">
                        <span>{typeLabel}</span>
                        {item.provider ? <span className="meta-text">{item.provider}</span> : null}
                      </div>
                      <p>{item.prompt}</p>
                    </div>
                  );
                })}
              </div>

              <div className="row">
                <button
                  className="button"
                  disabled={refine.isSubmittingInput}
                  onClick={() => {
                    void refine.respondToMediaApproval('generate');
                  }}
                  type="button"
                >
                  Generate assets
                </button>
                <button
                  className="button secondary"
                  disabled={refine.isSubmittingInput}
                  onClick={() => {
                    void refine.respondToMediaApproval('skip');
                  }}
                  type="button"
                >
                  Skip for now
                </button>
              </div>
            </div>
          ) : null}
        </section>
      </main>
    </div>
  );
}
