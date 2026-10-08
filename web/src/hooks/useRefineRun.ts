import { canContinueRefineSession } from '../lib/refine-session';
import { useJevForBuild } from '../lib/build-settings';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { ApiError, companionApi, openCodeApi, toErrorMessage, type CompanionBuildJob } from '../lib/api';
import { canonicalProjectPath } from '../lib/chat';
import { streamConfirmedSse } from '../lib/sse';
import { parseBuildStatusUpdate, type BuildStatusUpdate } from '../lib/build-status';
import { normalizeMediaApproval } from '../lib/media-approval';
import { agentPermissionResponses, normalizePermissionResponses } from '../lib/agent-permissions';
import { buildMediaApprovalResponse } from '../lib/build-media-approval';
import { companionJobIsTerminal, RefineRunConnection } from '../lib/refine-run-connection';
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
  permissionMode?: 'ask' | 'all';
  onAccepted?(): void;
  agentDriver?: 'opencode' | 'cursor' | 'claude-code' | 'codex' | 'acp';
  agentName?: string;
  projectPath: string;
  instructions?: string;
  buildTargets?: string[];
  persistCurrentInstructionsToHistory?: boolean;
  instructionAttachmentPaths?: string[];
  model?: string;
  reasoningEffort?: string;
  openaiAuthMode: OpenAIAuthMode;
  openaiRefreshToken?: string;
  openaiExpiresAt?: number;
  providerKeys: ProviderKeyState;
  elevenLabsUseSavedKey?: boolean;
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

function validRunID(value: unknown): string {
  const candidate = toStringValue(value).trim();
  return /^[A-Za-z0-9_-]{1,128}$/.test(candidate) ? candidate : '';
}

function extractSessionIDFromText(text: string): string {
  const match = text.match(/Session (?:created|resumed):\s*([A-Za-z0-9_-]+(?::[A-Za-z0-9_-]+){0,3})/i);
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
    availableResponses: normalizePermissionResponses(record.availableResponses, sessionID, toStringValue(record.id)),
    buildApprovalTool: toStringValue(record.buildApprovalTool) || undefined,
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
  const [liveStatuses, setLiveStatuses] = useState<BuildStatusUpdate[]>([]);
  const [pendingQuestion, setPendingQuestion] = useState<OpenCodeQuestion | null>(null);
  const [pendingPermission, setPendingPermission] = useState<OpenCodePermission | null>(null);
  const [pendingMediaApproval, setPendingMediaApproval] = useState<OpenCodeMediaApproval | null>(null);
  const [isSubmittingInput, setIsSubmittingInput] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [summary, setSummary] = useState<string | null>(null);
  const [resultText, setResultText] = useState('');
  const [changedFiles, setChangedFiles] = useState<string[]>([]);
  const [continueSession, setContinueSession] = useState(true);
  const [hasSession, setHasSession] = useState(false);
  const [request, setRequest] = useState('');
  const [runModel, setRunModel] = useState('');
  const [runPermissionMode, setRunPermissionMode] = useState<'ask' | 'all'>('ask');
  const [runAgentName, setRunAgentName] = useState('');
  const [runReasoningEffort, setRunReasoningEffort] = useState('');
  const [runId, setRunId] = useState('');
  const [jobId, setJobId] = useState('');
  const [isConnected, setIsConnected] = useState(false);
  const [isStopping, setIsStopping] = useState(false);
  const [runProjectPath, setRunProjectPath] = useState('');
  const [isSteerable, setIsSteerable] = useState(false);
  const [hasBeenSteerable, setHasBeenSteerable] = useState(false);
  const [startedAt, setStartedAt] = useState<number | null>(null);
  const [finishedAt, setFinishedAt] = useState<number | null>(null);

  const currentProjectPathRef = useRef('');
  const currentDriverRef = useRef('opencode');
  const currentModelRef = useRef('');
  const lastSessionIDRef = useRef('');
  const resolvedDecisionIDsRef = useRef(new Set<string>());
  const connectionRef = useRef<RefineRunConnection | null>(null);
  const runStartedAtRef = useRef<number | null>(null);
  const applyCompanionJobRef = useRef<(job: CompanionBuildJob) => boolean>(() => false);

  const createConnection = useCallback(() => {
    const connection = new RefineRunConnection({
      cancel: async id => {
        try { return await companionApi.cancelBuild(id); }
        catch (cause) {
          if (cause instanceof ApiError && cause.status === 409) return companionApi.getStatus();
          throw cause;
        }
      },
      onStopping: stopping => { if (connectionRef.current === connection) setIsStopping(stopping); },
      onCancelled: job => {
        if (connectionRef.current !== connection) return;
        applyCompanionJobRef.current(job);
        setIsConnected(false);
      },
      onCancelError: cause => {
        if (connectionRef.current === connection) setError(toErrorMessage(cause, 'Could not confirm that Desktop stopped this build. Check the connection and try again.'));
      },
    });
    return connection;
  }, []);

  useEffect(() => () => { connectionRef.current?.dispose(); connectionRef.current = null; }, []);

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

  const reconcileCompanionDecisions = useCallback((decisions: { kind: 'permission' | 'question' | 'media'; id: string }[]) => {
    for (const decision of decisions) if (decision.id) resolvedDecisionIDsRef.current.add(`${decision.kind}/${decision.id}`);
    const permissions = new Set(decisions.filter(decision => decision.kind === 'permission').map(decision => decision.id));
    const questions = new Set(decisions.filter(decision => decision.kind === 'question').map(decision => decision.id));
    const media = new Set(decisions.filter(decision => decision.kind === 'media').map(decision => decision.id));
    setPendingPermission(current => current?.id && permissions.has(current.id) ? null : current);
    setPendingQuestion(current => current?.id && questions.has(current.id) ? null : current);
    setPendingMediaApproval(current => current?.id && media.has(current.id) ? null : current);
  }, []);

  const applyCompanionJob = useCallback((job: CompanionBuildJob): boolean => {
    const id = validRunID(job.id);
    if (!id || job.kind === 'image') return false;
    let connection = connectionRef.current;
    if (connection?.jobId && connection.jobId !== id) return false;
    if (connection && !connection.jobId && currentProjectPathRef.current && canonicalProjectPath(currentProjectPathRef.current) !== canonicalProjectPath(job.projectPath)) return false;
    if (!connection) {
      connection = createConnection();
      connectionRef.current = connection;
    }
    if (!connection.identify(id)) return false;
    setJobId(id);
    reconcileCompanionDecisions(job.resolvedDecisions || []);
    const terminal = companionJobIsTerminal(job);
    if (job.permissionMode === 'all' || job.permissionMode === 'ask') setRunPermissionMode(current => current === 'all' ? current : job.permissionMode!);
    if (connection.connected && !terminal) return true;
    if (connection.terminal && !terminal) return false;

    currentProjectPathRef.current = job.projectPath;
    currentDriverRef.current = job.agentDriver || 'opencode';
    currentModelRef.current = job.model || '';
    lastSessionIDRef.current = job.sessionID || '';
    setHasSession(!!lastSessionIDRef.current);
    setRunProjectPath(job.projectPath);
    setRunId(validRunID(job.runId));
    setRequest(job.instructions || '');
    setRunModel(job.model || '');
    setRunAgentName(job.agentName || '');
    setRunReasoningEffort(job.reasoningEffort || '');
    setLogs((job.output || []).filter((line): line is string => typeof line === 'string').slice(-1000));
    setPartialLine(job.partialLine || '');
    const updates = (job.liveStatuses || (job.buildStatus ? [job.buildStatus] : [])).map(parseBuildStatusUpdate).filter((update): update is BuildStatusUpdate => !!update);
    setLiveStatuses(updates.slice(-12));
    setChangedFiles(sanitizeChangedFiles(job.changedFiles));
    setResultText(job.resultText || '');
    const start = Date.parse(job.startedAt);
    runStartedAtRef.current = Number.isFinite(start) ? start : null;
    setStartedAt(runStartedAtRef.current);
    const finish = Date.parse(job.finishedAt || '');
    setFinishedAt(terminal ? Number.isFinite(finish) ? finish : Date.now() : null);
    const permission = normalizePermission(job.pendingPermission, lastSessionIDRef.current);
    const question = normalizeQuestion(job.pendingQuestion, lastSessionIDRef.current);
    const media = normalizeMediaApproval(job.pendingMediaApproval);
    setPendingPermission(terminal || (permission?.id && resolvedDecisionIDsRef.current.has(`permission/${permission.id}`)) ? null : permission);
    setPendingQuestion(terminal || (question?.id && resolvedDecisionIDsRef.current.has(`question/${question.id}`)) ? null : question);
    setPendingMediaApproval(terminal || (media?.id && resolvedDecisionIDsRef.current.has(`media/${media.id}`)) ? null : media);
    const nextStatus: RefineRunStatus = job.status === 'completed' ? 'completed' : job.status === 'failed' ? 'failed' : terminal ? 'cancelled' : 'running';
    setStatus(nextStatus);
    setError(nextStatus === 'failed' ? userFacingAgentErrorMessage(job.error || 'Build failed.') : terminal ? null : job.error || null);
    setSummary(nextStatus === 'completed' ? 'Build completed.' : nextStatus === 'cancelled' ? 'Build stopped.' : nextStatus === 'failed' ? userFacingAgentErrorMessage(job.error || 'Build failed.') : null);
    if (terminal) {
      connection.complete(job.status);
      setIsSubmittingInput(false);
      setIsConnected(false);
      setIsSteerable(false);
    }
    return true;
  }, [createConnection, reconcileCompanionDecisions]);
  applyCompanionJobRef.current = applyCompanionJob;

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
      if (event.permissionMode === 'all' || event.permissionMode === 'ask') setRunPermissionMode(current => current === 'all' ? current : event.permissionMode!);
      if (typeof event.agentName === 'string') setRunAgentName(event.agentName.trim().slice(0, 160));
      if (typeof event.sessionID === 'string' && event.sessionID.length <= 2048) {
        lastSessionIDRef.current = event.sessionID;
        setHasSession(!!event.sessionID);
      }
      const streamedRunID = validRunID(event.runId);
      if (streamedRunID) setRunId(streamedRunID);
      if (typeof event.steerable === 'boolean') {
        setIsSteerable(event.steerable);
        if (event.steerable) setHasBeenSteerable(true);
      }
      if (event.status) {
        const update = parseBuildStatusUpdate(event.status);
        if (update) setLiveStatuses((previous) => {
          if (previous.at(-1)?.text === update.text) return previous;
          return [...previous, update].slice(-12);
        });
      }
      if (event.question) {
        const question = normalizeQuestion(event.question, lastSessionIDRef.current);
        if (question && !resolvedDecisionIDsRef.current.has(`question/${question.id}`)) {
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
        if (permission && !resolvedDecisionIDsRef.current.has(`permission/${permission.id}`)) {
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
        if (approval && !resolvedDecisionIDsRef.current.has(`media/${approval.id}`)) {
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
      if (typeof event.resultText === 'string') {
        setResultText(event.resultText.trim());
      }

      if (event.changedFiles) {
        updateChangedFiles(event.changedFiles);
      }

      if (typeof event.error === 'string' && event.error.trim() && !event.done) {
        setError(userFacingAgentErrorMessage(event.error));
      }

      if (event.done) {
        setIsSubmittingInput(false);
        setFinishedAt(Date.now());
        setIsSteerable(false);
        flushPartialLine();
        setPendingPermission(null);
        setPendingQuestion(null);
        setPendingMediaApproval(null);

        if (event.cancelled || event.jobStatus === 'canceled' || event.jobStatus === 'cancelled') {
          setStatus('cancelled');
          setError(null);
          setSummary('Build stopped.');
          return;
        }

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
      const permissionMode = input.permissionMode === 'all' ? 'all' : 'ask';
      const onAccepted = input.onAccepted;
      const projectPath = input.projectPath.trim();
      if (!projectPath) {
        setStatus('failed');
        setError('Project path is required.');
        setSummary('Project path is required.');
        return;
      }

      const model = (input.model || '').trim();

      connectionRef.current?.dispose();
      connectionRef.current = null;
      resolvedDecisionIDsRef.current.clear();

      // ACP connections keep independent sessions when the selected connection changes.
      if (!canContinueRefineSession({ projectPath: currentProjectPathRef.current, driver: currentDriverRef.current, model: currentModelRef.current }, { projectPath, driver: input.agentDriver || 'opencode', model }, continueSession)) {
        lastSessionIDRef.current = '';
        setHasSession(false);
      }
      currentProjectPathRef.current = projectPath;
      currentDriverRef.current = input.agentDriver || 'opencode';
      currentModelRef.current = model;
      setRunProjectPath(projectPath);

      setStatus('running');
      setRunPermissionMode('ask');
      runStartedAtRef.current = Date.now();
      setStartedAt(runStartedAtRef.current);
      setFinishedAt(null);
      setIsSteerable(false);
      setHasBeenSteerable(false);
      setError(null);
      setSummary(null);
      setResultText('');
      setLogs([]);
      setPartialLine('');
      setLiveStatuses([]);
      setPendingQuestion(null);
      setPendingPermission(null);
      setPendingMediaApproval(null);
      setChangedFiles([]);
      setIsSubmittingInput(false);
      setIsStopping(false);
      setIsConnected(false);
      setJobId('');

      const payload: OpenCodeAgentRequest = {
        useJev: useJevForBuild(input.agentDriver),
        agentDriver: input.agentDriver,
        permissionMode,
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
      if (input.agentDriver === 'codex' && input.reasoningEffort) payload.reasoningEffort = input.reasoningEffort;

      const trimmedInstructions = input.instructions?.trim() || '';
      setRequest(trimmedInstructions);
      setRunModel(model);
      setRunAgentName(input.agentDriver === 'acp' ? input.agentName || '' : '');
      setRunReasoningEffort(input.agentDriver === 'codex' ? input.reasoningEffort || '' : '');
      setRunId('');
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
      } else if (input.elevenLabsUseSavedKey === true) {
        payload.elevenLabsUseSavedKey = true;
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

      const connection = createConnection();
      connectionRef.current = connection;

      const identifyJob = (id: unknown) => {
        const value = validRunID(id);
        if (connectionRef.current === connection && value && connection.identify(value)) setJobId(value);
      };

      try {
        const completion = await streamConfirmedSse<OpenCodeAgentRequest, OpenCodeSseEvent>({
          url: '/api/opencode/refine',
          body: payload,
          signal: connection.controller.signal,
          onEvent: event => {
            if (connectionRef.current !== connection) return;
            identifyJob(event.jobId);
            if (connection.terminal && !event.done) return;
            if (connection.terminalStatus && event.done) return;
            handleSseEvent(event);
            if (event.done) { connection.complete(); setIsConnected(false); }
          },
          onResponse: response => {
            if (connectionRef.current !== connection) return;
            connection.accepted = true;
            setRunPermissionMode(permissionMode);
            onAccepted?.();
            connection.connected = true;
            setIsConnected(true);
            identifyJob(response.headers.get('X-Glowbom-Job-ID'));
            const headerRunID = validRunID(response.headers.get('X-Glowbom-Run-ID'));
            if (headerRunID) setRunId(headerRunID);
          },
        });

        if (connectionRef.current !== connection) return;
        flushPartialLine();
        if (completion.success === true) {
          setFinishedAt((previous) => previous ?? Date.now());
          setStatus((previous) => (previous === 'running' ? 'completed' : previous));
          setSummary((previous) => previous || 'Build completed.');
        }
      } catch (requestError) {
        if (connectionRef.current !== connection || connection.terminal) return;
        connection.connected = false;
        setIsConnected(false);
        setIsSteerable(false);

        if (connection.accepted || isAbortError(requestError) || requestError instanceof TypeError) {
          if (!connection.detached) setError(connection.accepted
            ? 'The progress connection was interrupted. Desktop keeps building; reconnect to see its progress.'
            : 'Could not confirm the build connection. Reconnect to Desktop and check its progress before starting another build.');
        } else {
          connection.complete();
          setFinishedAt(Date.now());
          flushPartialLine();
          const message = toErrorMessage(requestError, 'Build failed.');
          setStatus('failed');
          setError(message);
          setSummary(message);
          appendLine(`❌ ${message}`);
        }
      } finally {
        if (connectionRef.current === connection) {
          connection.connected = false;
          setIsConnected(false);
          setIsSteerable(false);
        }
      }
    },
    [appendLine, continueSession, createConnection, flushPartialLine, handleSseEvent],
  );

  const stopRun = useCallback(() => {
    setIsSteerable(false);
    connectionRef.current?.requestStop();
  }, []);

  const disconnectRun = useCallback(() => {
    connectionRef.current?.disconnect();
    setIsConnected(false);
    setIsSteerable(false);
  }, []);

  const submitQuestion = useCallback(
    async (input: SubmitQuestionInput) => {
      if (!pendingQuestion || isSubmittingInput) {
        return false;
      }

      const question = pendingQuestion;
      const connection = connectionRef.current;
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
        if (connectionRef.current !== connection) return false;
        resolvedDecisionIDsRef.current.add(`question/${question.id}`);
        setPendingQuestion(null);
        setPendingPermission(null);
        setError(null);

        const trimmed = input.answer.trim();
        appendLine(trimmed ? `A: ${trimmed}` : 'A: (dismissed)');
        return true;
      } catch (requestError) {
        if (connectionRef.current !== connection) return false;
        const message = toErrorMessage(requestError, 'Failed to send answer.');
        setError(message);
        appendLine(`❌ ${message}`);
        return false;
      } finally {
        if (connectionRef.current === connection) setIsSubmittingInput(false);
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
      if (!agentPermissionResponses(permission).includes(response)) {
        setError('This permission choice is no longer available. Choose one of the options shown.');
        return false;
      }
      const connection = connectionRef.current;
      const payload: OpenCodePermissionRespondRequest = {
        sessionID: permission.sessionID,
        permissionID: permission.id,
        response,
        projectPath: currentProjectPathRef.current || undefined,
      };

      setIsSubmittingInput(true);
      try {
        const result = await openCodeApi.respondToPermission(payload);
        if (!result.ok) throw new Error('Desktop could not confirm this permission response.');
        if (connectionRef.current !== connection) return false;
        resolvedDecisionIDsRef.current.add(`permission/${permission.id}`);
        if (response === 'all') setRunPermissionMode('all');
        setPendingPermission((current) => current?.id === permission.id && current.sessionID === permission.sessionID ? null : current);
        setError(null);
        appendLine(`Permission response sent: ${response}`);
        return true;
      } catch (requestError) {
        if (connectionRef.current !== connection) return false;
        const message = toErrorMessage(requestError, 'Failed to send permission response.');
        setError(message);
        appendLine(`❌ ${message}`);
        return false;
      } finally {
        if (connectionRef.current === connection) setIsSubmittingInput(false);
      }
    },
    [appendLine, isSubmittingInput, pendingPermission],
  );

  const respondToMediaApproval = useCallback(
    async (response: OpenCodeMediaApprovalRespondRequest['response'], items?: OpenCodeMediaApprovalItem[]) => {
      if (!pendingMediaApproval || isSubmittingInput) {
        return false;
      }

      const approval = pendingMediaApproval;
      const connection = connectionRef.current;
      setIsSubmittingInput(true);
      try {
        await openCodeApi.respondToMediaApproval(buildMediaApprovalResponse(approval, response, currentProjectPathRef.current, items || approval.items));
        if (connectionRef.current !== connection) return false;
        resolvedDecisionIDsRef.current.add(`media/${approval.id}`);
        setPendingMediaApproval(null);
        setError(null);
        appendLine(response === 'generate' ? 'Media generation approved.' : 'Media generation skipped.');
        return true;
      } catch (requestError) {
        if (connectionRef.current !== connection) return false;
        const message = toErrorMessage(requestError, 'Failed to send media approval response.');
        setError(message);
        appendLine(`❌ ${message}`);
        return false;
      } finally {
        if (connectionRef.current === connection) setIsSubmittingInput(false);
      }
    },
    [appendLine, isSubmittingInput, pendingMediaApproval],
  );

  return useMemo(
    () => ({
      status,
      logs,
      partialLine,
      liveStatuses,
      pendingQuestion,
      pendingPermission,
      pendingMediaApproval,
      isSubmittingInput: isSubmittingInput || isStopping,
      isStopping,
      isConnected,
      error,
      summary,
      resultText,
      changedFiles,
      isRunning: status === 'running',
      isSteerable,
      hasBeenSteerable,
      startedAt,
      finishedAt,
      isAwaitingInput: pendingQuestion !== null || pendingPermission !== null || pendingMediaApproval !== null,
      hasSession,
      request,
      model: runModel,
      permissionMode: runPermissionMode,
      agentName: runAgentName,
      reasoningEffort: runReasoningEffort,
      runId,
      jobId,
      projectPath: runProjectPath,
      continueSession,
      setContinueSession,
      startRefine,
      stopRun,
      disconnectRun,
      applyCompanionJob,
      submitQuestion,
      respondToPermission,
      respondToMediaApproval,
      reconcileCompanionDecisions,
    }),
    [
      changedFiles,
      continueSession,
      error,
      hasSession,
      isSubmittingInput,
      isStopping,
      isConnected,
      isSteerable,
      hasBeenSteerable,
      startedAt,
      finishedAt,
      logs,
      partialLine,
      liveStatuses,
      pendingPermission,
      pendingMediaApproval,
      pendingQuestion,
      request,
      runModel,
      runPermissionMode,
      runAgentName,
      runReasoningEffort,
      runId,
      jobId,
      runProjectPath,
      respondToPermission,
      respondToMediaApproval,
      reconcileCompanionDecisions,
      startRefine,
      applyCompanionJob,
      disconnectRun,
      status,
      stopRun,
      submitQuestion,
      summary,
      resultText,
    ],
  );
}

export type RefineRun = ReturnType<typeof useRefineRun>;
