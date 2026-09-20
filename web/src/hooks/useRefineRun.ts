import { useCallback, useMemo, useRef, useState } from 'react';
import { openCodeApi, toErrorMessage } from '../lib/api';
import { streamJsonSse } from '../lib/sse';
import type {
  OpenCodeAgentRequest,
  OpenCodeMediaApproval,
  OpenCodeMediaApprovalItem,
  OpenCodeMediaApprovalRespondRequest,
  OpenCodePermission,
  OpenCodePermissionRespondRequest,
  OpenCodeQuestion,
  OpenCodeQuestionItem,
  OpenCodeQuestionOption,
  OpenCodeQuestionRespondRequest,
  OpenCodeSseEvent,
  OpenAIAuthMode,
  ImageProviderKeyState,
  ProviderKeyState,
} from '../types/opencode';

export type RefineRunStatus = 'idle' | 'running' | 'completed' | 'failed' | 'cancelled';

export interface StartRefineInput {
  agentDriver?: 'opencode' | 'cursor';
  projectPath: string;
  instructions?: string;
  buildTargets?: string[];
  persistCurrentInstructionsToHistory?: boolean;
  instructionAttachmentPaths?: string[];
  model?: string;
  openaiAuthMode: OpenAIAuthMode;
  openaiRefreshToken?: string;
  openaiExpiresAt?: number;
  providerKeys: ProviderKeyState;
  imageProviderKeys: ImageProviderKeyState;
  imageSource?: string;
}

export interface SubmitQuestionInput {
  answer: string;
  answers?: string[][];
  answerByQuestionID?: Record<string, string[]>;
}

function toStringValue(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function extractSessionIDFromText(text: string): string {
  const match = text.match(/Session (?:created|resumed):\s*([A-Za-z0-9_-]+)/i);
  return match?.[1] || '';
}

function normalizeIncomingOutputText(text: string): string {
  if (!text) {
    return '';
  }

  return text
    .replaceAll('\r\n', '\n')
    .replaceAll('\r', '\n')
    .replaceAll('\u2028', '\n')
    .replaceAll('\u2029', '\n')
    .replaceAll('\\r\\n', '\n')
    .replaceAll('\\n', '\n')
    .replaceAll('\\u000A', '\n')
    .replaceAll('\\u2028', '\n')
    .replaceAll('\\u2029', '\n');
}

function firstRegexInt(text: string, regex: RegExp): number | undefined {
  const match = regex.exec(text);
  if (!match || !match[1]) {
    return undefined;
  }
  const parsed = Number.parseInt(match[1], 10);
  return Number.isFinite(parsed) ? parsed : undefined;
}

function usageLimitWindowMinutes(text: string): number | undefined {
  return firstRegexInt(text, /(?:x-codex-primary-window-minutes|window[_-]?minutes)["']?\s*[:=]\s*["']?(\d{1,10})/i);
}

function usageLimitUsedPercent(text: string): number | undefined {
  return firstRegexInt(text, /(?:x-codex-primary-used-percent|used[_-]?percent)["']?\s*[:=]\s*["']?(\d{1,3})/i);
}

function usageLimitResetDate(text: string): Date | undefined {
  const epoch = firstRegexInt(text, /(?:resets[_-]?at|reset[_-]?at|x-codex-primary-reset-at)["']?\s*[:=]\s*["']?(\d{9,})/i);
  if (typeof epoch === 'number' && epoch > 0) {
    return new Date(epoch * 1000);
  }

  const seconds = firstRegexInt(
    text,
    /(?:resets[_-]?in[_-]?seconds|reset[_-]?after[_-]?seconds|x-codex-primary-reset-after-seconds)["']?\s*[:=]\s*["']?(\d{1,10})/i,
  );
  if (typeof seconds === 'number' && seconds > 0) {
    return new Date(Date.now() + seconds * 1000);
  }

  return undefined;
}

function formatQuotaWindow(minutes: number): string {
  if (minutes <= 0) {
    return 'quota';
  }
  if (minutes % (24 * 60) === 0) {
    return `${minutes / (24 * 60)}-day`;
  }
  if (minutes % 60 === 0) {
    return `${minutes / 60}-hour`;
  }
  return `${minutes}-minute`;
}

function relativeDurationString(untilDate: Date): string | undefined {
  const seconds = Math.round((untilDate.getTime() - Date.now()) / 1000);
  if (seconds <= 0) {
    return undefined;
  }

  let totalMinutes = Math.floor(seconds / 60);
  if (totalMinutes <= 0) {
    return '<1m';
  }

  const days = Math.floor(totalMinutes / (24 * 60));
  totalMinutes %= 24 * 60;
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;

  const parts: string[] = [];
  if (days > 0) {
    parts.push(`${days}d`);
  }
  if (hours > 0) {
    parts.push(`${hours}h`);
  }
  if (minutes > 0 && parts.length < 2) {
    parts.push(`${minutes}m`);
  }

  return parts.length > 0 ? parts.join(' ') : '<1m';
}

function usageLimitFriendlyMessage(text: string): string | undefined {
  const lower = text.toLowerCase();
  const isUsageLimit =
    lower.includes('usage_limit_reached') ||
    lower.includes('usage limit has been reached') ||
    lower.includes('too many requests') ||
    lower.includes('rate limit') ||
    lower.includes('x-codex-primary-used-percent":"100"');

  if (!isUsageLimit) {
    return undefined;
  }

  let message =
    'Usage limit reached for the selected model on the current plan. Try another model, or retry after your quota resets.';

  const usedPercent = usageLimitUsedPercent(text);
  if (typeof usedPercent === 'number') {
    const windowMinutes = usageLimitWindowMinutes(text);
    if (typeof windowMinutes === 'number' && windowMinutes > 0) {
      message += ` Current usage: ${usedPercent}% of your ${formatQuotaWindow(windowMinutes)} quota window.`;
    } else {
      message += ` Current usage: ${usedPercent}% of your quota window.`;
    }
  }

  const resetDate = usageLimitResetDate(text);
  if (resetDate) {
    const formattedReset = new Intl.DateTimeFormat(undefined, {
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      timeZoneName: 'short',
    }).format(resetDate);

    const remaining = relativeDurationString(resetDate);
    if (remaining) {
      message += ` Quota resets at ${formattedReset} (in about ${remaining}).`;
    } else {
      message += ` Quota resets at ${formattedReset}.`;
    }
  }

  return message;
}

function normalizedAgentOutputLine(line: string): string {
  const friendly = usageLimitFriendlyMessage(line);
  if (friendly) {
    return `❌ ${friendly}`;
  }
  return line;
}

function userFacingAgentErrorMessage(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed) {
    return 'Unknown error';
  }

  const friendly = usageLimitFriendlyMessage(trimmed);
  if (friendly) {
    return friendly;
  }

  if (trimmed.length > 280) {
    return `${trimmed.slice(0, 280)}...`;
  }

  return trimmed;
}

function isAlphaNumericCharacter(character: string): boolean {
  return /^[\p{L}\p{N}]$/u.test(character);
}

function isWhitespaceCharacter(character: string): boolean {
  return /^\s$/u.test(character);
}

function inferredChunkSeparator(currentLine: string, nextPart: string): '' | ' ' | '\n' {
  if (!currentLine || !nextPart) {
    return '';
  }

  if (currentLine.endsWith('**') && nextPart.startsWith('**')) {
    return '\n';
  }

  const trimmedNext = nextPart.trim();
  if (trimmedNext.startsWith('#') && currentLine.trim()) {
    return '\n';
  }

  // We do NOT guess whether to insert spaces or swallow empty strings for streaming chunks.
  // The LLM provides correct whitespace in `message.part.delta`.
  return '';
}

function parseQuestionOptions(raw: unknown): OpenCodeQuestionOption[] {
  if (!Array.isArray(raw)) {
    return [];
  }

  return raw
    .map((item, index) => {
      if (!item || typeof item !== 'object') {
        return null;
      }

      const option = item as Record<string, unknown>;
      const label = toStringValue(option.label);
      if (!label) {
        return null;
      }

      return {
        id: toStringValue(option.id) || `option-${index}`,
        label,
        description: toStringValue(option.description),
      } satisfies OpenCodeQuestionOption;
    })
    .filter((item): item is OpenCodeQuestionOption => item !== null);
}

function normalizeQuestion(raw: unknown, fallbackSessionID: string): OpenCodeQuestion | null {
  if (!raw || typeof raw !== 'object') {
    return null;
  }

  const record = raw as Record<string, unknown>;
  const nestedQuestionsRaw = Array.isArray(record.questions) ? record.questions : [];

  const nestedQuestions = nestedQuestionsRaw
    .map((questionItem, index): OpenCodeQuestionItem | null => {
      if (!questionItem || typeof questionItem !== 'object') {
        return null;
      }

      const item = questionItem as Record<string, unknown>;
      const prompt =
        toStringValue(item.prompt) ||
        toStringValue(item.question) ||
        toStringValue(item.text) ||
        `Question ${index + 1}`;

      return {
        id: toStringValue(item.id) || `question-${index}`,
        prompt,
        options: parseQuestionOptions(item.choices ?? item.options),
        inputType: toStringValue(item.type) || undefined,
      };
    })
    .filter((item): item is OpenCodeQuestionItem => item !== null);

  const sessionID =
    toStringValue(record.sessionID) ||
    toStringValue(record.sessionId) ||
    toStringValue(record.session_id) ||
    fallbackSessionID;

  return {
    id: toStringValue(record.id),
    sessionID,
    prompt: toStringValue(record.prompt) || 'Agent asked a question.',
    options: parseQuestionOptions(record.choices ?? record.options),
    questions: nestedQuestions,
  };
}

function normalizePermission(raw: unknown, fallbackSessionID: string): OpenCodePermission | null {
  if (!raw || typeof raw !== 'object') {
    return null;
  }

  const record = raw as Record<string, unknown>;
  const sessionID =
    toStringValue(record.sessionID) ||
    toStringValue(record.sessionId) ||
    toStringValue(record.session_id) ||
    fallbackSessionID;

  return {
    id: toStringValue(record.id),
    sessionID,
    title: toStringValue(record.title) || 'Permission requested',
    type: toStringValue(record.type),
    message: toStringValue(record.message),
    pattern: toStringValue(record.pattern),
  };
}

function normalizeMediaApproval(raw: unknown): OpenCodeMediaApproval | null {
  if (!raw || typeof raw !== 'object') {
    return null;
  }

  const record = raw as Record<string, unknown>;
  const id = toStringValue(record.id);
  if (!id) {
    return null;
  }

  const rawItems = Array.isArray(record.items) ? record.items : [];
  const items = rawItems
    .map((rawItem): OpenCodeMediaApprovalItem | null => {
      if (!rawItem || typeof rawItem !== 'object') {
        return null;
      }
      const item = rawItem as Record<string, unknown>;
      const prompt = toStringValue(item.prompt);
      const mediaType = toStringValue(item.mediaType);
      if (!prompt || !mediaType) {
        return null;
      }
      return {
        mediaType,
        prompt,
        provider: toStringValue(item.provider),
        audioType: toStringValue(item.audioType) || undefined,
      };
    })
    .filter((item): item is OpenCodeMediaApprovalItem => item !== null);

  if (items.length === 0) {
    return null;
  }

  return {
    id,
    title: toStringValue(record.title) || 'Generate new media assets?',
    message: toStringValue(record.message),
    items,
  };
}

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError';
}

function sanitizeChangedFiles(files: unknown): string[] {
  if (!Array.isArray(files)) {
    return [];
  }

  return files.filter((item): item is string => typeof item === 'string' && item.trim().length > 0);
}

function trimmedValue(value: string | undefined): string {
  return (value || '').trim();
}

export function useRefineRun() {
  const [status, setStatus] = useState<RefineRunStatus>('idle');
  const [logs, setLogs] = useState<string[]>([]);
  const [partialLine, setPartialLine] = useState('');
  const [pendingQuestion, setPendingQuestion] = useState<OpenCodeQuestion | null>(null);
  const [pendingPermission, setPendingPermission] = useState<OpenCodePermission | null>(null);
  const [pendingMediaApproval, setPendingMediaApproval] = useState<OpenCodeMediaApproval | null>(null);
  const [isSubmittingInput, setIsSubmittingInput] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [summary, setSummary] = useState<string | null>(null);
  const [changedFiles, setChangedFiles] = useState<string[]>([]);
  const [continueSession, setContinueSession] = useState(true);
  const [hasSession, setHasSession] = useState(false);

  const currentProjectPathRef = useRef('');
  const currentDriverRef = useRef('opencode');
  const lastSessionIDRef = useRef('');
  const abortControllerRef = useRef<AbortController | null>(null);

  const appendLines = useCallback((incoming: string[]) => {
    if (incoming.length === 0) {
      return;
    }

    setLogs((previous) => {
      const next = [...previous];
      for (const rawLine of incoming) {
        const line = rawLine.replaceAll('\r', '');
        if (!line) {
          continue;
        }
        if (next[next.length - 1] === line) {
          continue;
        }
        next.push(line);
      }
      return next.length > 1000 ? next.slice(-1000) : next;
    });
  }, []);

  const appendLine = useCallback(
    (line: string) => {
      appendLines([line]);
    },
    [appendLines],
  );

  const flushPartialLine = useCallback(() => {
    setPartialLine((previous) => {
      if (previous.trim()) {
        appendLines([previous]);
      }
      return '';
    });
  }, [appendLines]);

  const updateChangedFiles = useCallback((files: unknown) => {
    setChangedFiles(sanitizeChangedFiles(files));
  }, []);

  const handleOutputLine = useCallback((line: string) => {
    const sessionID = extractSessionIDFromText(line);
    if (sessionID) {
      lastSessionIDRef.current = sessionID;
      setHasSession(true);
    }

    const lower = line.toLowerCase();
    if (currentDriverRef.current === 'opencode' && (lower.includes('agent needs input') || lower.startsWith('❓ question:'))) {
      setPendingQuestion((existing) => {
        if (existing) {
          return existing;
        }

        const prompt = line.replace(/^❓\s*question:\s*/i, '').trim();
        return {
          id: '',
          sessionID: lastSessionIDRef.current,
          prompt: prompt || 'Agent asked a question.',
          options: [],
          questions: [],
        };
      });
      setPendingPermission(null);
    }
  }, []);

  const appendOutputChunk = useCallback(
    (chunk: string) => {
      const normalized = normalizeIncomingOutputText(chunk);
      if (!normalized) {
        return;
      }

      setPartialLine((previous) => {
        let activeLine = previous;
        const completedLines: string[] = [];
        const parts = normalized.split('\n');

        for (let index = 0; index < parts.length; index += 1) {
          const part = parts[index] ?? '';
          const isLast = index === parts.length - 1;

          if (part) {
            const separator = inferredChunkSeparator(activeLine, part);
            if (separator === '\n') {
              if (activeLine) {
                completedLines.push(activeLine);
              }
              activeLine = part;
            } else if (separator === ' ') {
              activeLine += ' ' + part;
            } else {
              activeLine += part;
            }
          }

          if (!isLast) {
            if (activeLine) {
              completedLines.push(activeLine);
            }
            activeLine = '';
          }
        }

        if (completedLines.length > 0) {
          appendLines(completedLines);
        }

        return activeLine;
      });
    },
    [appendLines],
  );

  const appendOutputMessage = useCallback(
    (output: string) => {
      flushPartialLine();

      const normalized = normalizeIncomingOutputText(output);
      const lines = normalized.split('\n');
      if (lines.length > 0 && lines[lines.length - 1] === '') {
        lines.pop();
      }

      const normalizedLines: string[] = [];
      for (const rawLine of lines) {
        const line = normalizedAgentOutputLine(rawLine);
        handleOutputLine(line);
        normalizedLines.push(line);
      }

      if (normalizedLines.length > 0) {
        appendLines(normalizedLines);
      }

      const sessionID = extractSessionIDFromText(normalized);
      if (sessionID) {
        lastSessionIDRef.current = sessionID;
        setHasSession(true);
      }
    },
    [appendLines, flushPartialLine, handleOutputLine],
  );

  const handleSseEvent = useCallback(
    (event: OpenCodeSseEvent) => {
      if (event.question) {
        const question = normalizeQuestion(event.question, lastSessionIDRef.current);
        if (question) {
          if (question.sessionID) {
            lastSessionIDRef.current = question.sessionID;
          }
          setPendingQuestion(question);
          setPendingPermission(null);
          setPendingMediaApproval(null);
        }
      }

      if (event.permission) {
        const permission = normalizePermission(event.permission, lastSessionIDRef.current);
        if (permission) {
          if (permission.sessionID) {
            lastSessionIDRef.current = permission.sessionID;
          }
          setPendingPermission(permission);
          setPendingQuestion(null);
          setPendingMediaApproval(null);
        }
      }

      if (event.mediaApproval) {
        const approval = normalizeMediaApproval(event.mediaApproval);
        if (approval) {
          setPendingMediaApproval(approval);
          setPendingQuestion(null);
          setPendingPermission(null);
        }
      }

      if (typeof event.outputChunk === 'string') {
        appendOutputChunk(event.outputChunk);
      }

      if (typeof event.output === 'string') {
        appendOutputMessage(event.output);
      }

      if (event.changedFiles) {
        updateChangedFiles(event.changedFiles);
      }

      if (typeof event.error === 'string' && event.error.trim() && !event.done) {
        setError(userFacingAgentErrorMessage(event.error));
      }

      if (event.done) {
        flushPartialLine();
        setPendingPermission(null);
        setPendingQuestion(null);
        setPendingMediaApproval(null);

        if (event.success === true) {
          setStatus('completed');
          setError(null);

          const filesChanged = sanitizeChangedFiles(event.changedFiles);
          if (filesChanged.length > 0) {
            updateChangedFiles(filesChanged);
            setSummary(
              `Build completed. ${filesChanged.length} file${filesChanged.length === 1 ? '' : 's'} changed.`,
            );
          } else {
            setSummary('Build completed.');
          }
          return;
        }

        const message = userFacingAgentErrorMessage(toStringValue(event.error) || 'Build failed.');
        setStatus('failed');
        setError(message);
        setSummary(message);
        appendLine(`❌ ${message}`);
      }
    },
    [appendLine, appendOutputChunk, appendOutputMessage, flushPartialLine, updateChangedFiles],
  );

  const startRefine = useCallback(
    async (input: StartRefineInput) => {
      const projectPath = input.projectPath.trim();
      if (!projectPath) {
        setStatus('failed');
        setError('Project path is required.');
        setSummary('Project path is required.');
        return;
      }

      const model = (input.model || '').trim();

      abortControllerRef.current?.abort();

      // Clear session if toggle is off or project changed.
      if (!continueSession || currentProjectPathRef.current !== projectPath || currentDriverRef.current !== (input.agentDriver || 'opencode')) {
        lastSessionIDRef.current = '';
        setHasSession(false);
      }
      currentProjectPathRef.current = projectPath;
      currentDriverRef.current = input.agentDriver || 'opencode';

      setStatus('running');
      setError(null);
      setSummary(null);
      setLogs([]);
      setPartialLine('');
      setPendingQuestion(null);
      setPendingPermission(null);
      setPendingMediaApproval(null);
      setChangedFiles([]);
      setIsSubmittingInput(false);

      const payload: OpenCodeAgentRequest = {
        agentDriver: input.agentDriver,
        projectPath,
        openaiAuthMode: input.openaiAuthMode,
        mediaGenerationPolicy: 'ask',
        buildTargets: input.buildTargets,
      };
      if (lastSessionIDRef.current) {
        payload.sessionID = lastSessionIDRef.current;
      }
      if (input.persistCurrentInstructionsToHistory) {
        payload.persistCurrentInstructionsToHistory = true;
      }
      if (model) {
        payload.model = model;
      }

      const trimmedInstructions = input.instructions?.trim() || '';
      if (trimmedInstructions) {
        payload.instructions = trimmedInstructions;
      }

      const instructionAttachmentPaths = (input.instructionAttachmentPaths || [])
        .map((value) => value.trim())
        .filter((value, index, values) => value.length > 0 && values.indexOf(value) === index);
      if (instructionAttachmentPaths.length > 0) {
        payload.instructionAttachmentPaths = instructionAttachmentPaths;
      }

      const openaiKey = trimmedValue(input.providerKeys.openaiKey);
      const anthropicKey = trimmedValue(input.providerKeys.anthropicKey);
      const geminiKey = trimmedValue(input.providerKeys.geminiKey);
      const fireworksKey = trimmedValue(input.providerKeys.fireworksKey);
      const openrouterKey = trimmedValue(input.providerKeys.openrouterKey);
      const opencodeZenKey = trimmedValue(input.providerKeys.opencodeZenKey);
      const xaiKey = trimmedValue(input.providerKeys.xaiKey);
      const elevenLabsKey = trimmedValue(input.providerKeys.elevenLabsKey);
      const openaiImageKey = trimmedValue(input.imageProviderKeys.openaiImageKey);
      const geminiImageKey = trimmedValue(input.imageProviderKeys.geminiImageKey);
      const xaiImageKey = trimmedValue(input.imageProviderKeys.xaiImageKey);

      if (openaiKey) {
        payload.openaiKey = openaiKey;
      }
      if (anthropicKey) {
        payload.anthropicKey = anthropicKey;
      }
      if (geminiKey) {
        payload.geminiKey = geminiKey;
      }
      if (fireworksKey) {
        payload.fireworksKey = fireworksKey;
      }
      if (openrouterKey) {
        payload.openrouterKey = openrouterKey;
      }
      if (opencodeZenKey) {
        payload.opencodeZenKey = opencodeZenKey;
      }
      if (xaiKey) {
        payload.xaiKey = xaiKey;
      }
      if (elevenLabsKey) {
        payload.elevenLabsKey = elevenLabsKey;
      }
      if (openaiImageKey) {
        payload.openaiImageKey = openaiImageKey;
      }
      if (geminiImageKey) {
        payload.geminiImageKey = geminiImageKey;
      }
      if (xaiImageKey) {
        payload.xaiImageKey = xaiImageKey;
      }

      const imageSource = input.imageSource?.trim() || '';
      if (imageSource) {
        payload.imageSource = imageSource;
      }

      if (input.openaiAuthMode === 'codex-jwt' && openaiKey) {
        const refreshToken = input.openaiRefreshToken?.trim() || '';
        if (refreshToken) {
          payload.openaiRefreshToken = refreshToken;
        }
        if (typeof input.openaiExpiresAt === 'number' && Number.isFinite(input.openaiExpiresAt)) {
          payload.openaiExpiresAt = input.openaiExpiresAt;
        }
      }

      const controller = new AbortController();
      abortControllerRef.current = controller;

      try {
        await streamJsonSse<OpenCodeAgentRequest, OpenCodeSseEvent>({
          url: '/api/opencode/refine',
          body: payload,
          signal: controller.signal,
          onEvent: handleSseEvent,
        });

        flushPartialLine();
        setStatus((previous) => (previous === 'running' ? 'completed' : previous));
        setSummary((previous) => previous || 'Build completed.');
      } catch (requestError) {
        flushPartialLine();

        if (isAbortError(requestError)) {
          setStatus((previous) => (previous === 'running' ? 'cancelled' : previous));
          setSummary((previous) => previous || 'Build stopped.');
          appendLine('⏹️ Build stopped.');
        } else {
          const message = toErrorMessage(requestError, 'Build failed.');
          setStatus('failed');
          setError(message);
          setSummary(message);
          appendLine(`❌ ${message}`);
        }
      } finally {
        abortControllerRef.current = null;
      }
    },
    [appendLine, flushPartialLine, handleSseEvent],
  );

  const stopRun = useCallback(() => {
    abortControllerRef.current?.abort();
  }, []);

  const submitQuestion = useCallback(
    async (input: SubmitQuestionInput) => {
      if (!pendingQuestion || isSubmittingInput) {
        return false;
      }

      const question = pendingQuestion;
      const payload: OpenCodeQuestionRespondRequest = {
        sessionID: question.sessionID,
        questionID: question.id,
        answer: input.answer,
        projectPath: currentProjectPathRef.current || undefined,
      };

      if (input.answers && input.answers.length > 0) {
        payload.answers = input.answers;
      }

      if (input.answerByQuestionID && Object.keys(input.answerByQuestionID).length > 0) {
        payload.answerByQuestionID = input.answerByQuestionID;
      }

      setIsSubmittingInput(true);
      try {
        await openCodeApi.respondToQuestion(payload);
        setPendingQuestion(null);
        setPendingPermission(null);
        setError(null);

        const trimmed = input.answer.trim();
        appendLine(trimmed ? `A: ${trimmed}` : 'A: (dismissed)');
        return true;
      } catch (requestError) {
        const message = toErrorMessage(requestError, 'Failed to send answer.');
        setError(message);
        appendLine(`❌ ${message}`);
        return false;
      } finally {
        setIsSubmittingInput(false);
      }
    },
    [appendLine, isSubmittingInput, pendingQuestion],
  );

  const respondToPermission = useCallback(
    async (response: OpenCodePermissionRespondRequest['response']) => {
      if (!pendingPermission || isSubmittingInput) {
        return false;
      }

      const permission = pendingPermission;
      const payload: OpenCodePermissionRespondRequest = {
        sessionID: permission.sessionID,
        permissionID: permission.id,
        response,
        projectPath: currentProjectPathRef.current || undefined,
      };

      setIsSubmittingInput(true);
      try {
        await openCodeApi.respondToPermission(payload);
        setPendingPermission(null);
        setPendingQuestion(null);
        setError(null);
        appendLine(`Permission response sent: ${response}`);
        return true;
      } catch (requestError) {
        const message = toErrorMessage(requestError, 'Failed to send permission response.');
        setError(message);
        appendLine(`❌ ${message}`);
        return false;
      } finally {
        setIsSubmittingInput(false);
      }
    },
    [appendLine, isSubmittingInput, pendingPermission],
  );

  const respondToMediaApproval = useCallback(
    async (response: OpenCodeMediaApprovalRespondRequest['response']) => {
      if (!pendingMediaApproval || isSubmittingInput) {
        return false;
      }

      const approval = pendingMediaApproval;
      setIsSubmittingInput(true);
      try {
        await openCodeApi.respondToMediaApproval({
          approvalID: approval.id,
          response,
          projectPath: currentProjectPathRef.current || undefined,
        });
        setPendingMediaApproval(null);
        setError(null);
        appendLine(response === 'generate' ? 'Media generation approved.' : 'Media generation skipped.');
        return true;
      } catch (requestError) {
        const message = toErrorMessage(requestError, 'Failed to send media approval response.');
        setError(message);
        appendLine(`❌ ${message}`);
        return false;
      } finally {
        setIsSubmittingInput(false);
      }
    },
    [appendLine, isSubmittingInput, pendingMediaApproval],
  );

  return useMemo(
    () => ({
      status,
      logs,
      partialLine,
      pendingQuestion,
      pendingPermission,
      pendingMediaApproval,
      isSubmittingInput,
      error,
      summary,
      changedFiles,
      isRunning: status === 'running',
      isAwaitingInput: pendingQuestion !== null || pendingPermission !== null || pendingMediaApproval !== null,
      hasSession,
      continueSession,
      setContinueSession,
      startRefine,
      stopRun,
      submitQuestion,
      respondToPermission,
      respondToMediaApproval,
    }),
    [
      changedFiles,
      continueSession,
      error,
      hasSession,
      isSubmittingInput,
      logs,
      partialLine,
      pendingPermission,
      pendingMediaApproval,
      pendingQuestion,
      respondToPermission,
      respondToMediaApproval,
      startRefine,
      status,
      stopRun,
      submitQuestion,
      summary,
    ],
  );
}
