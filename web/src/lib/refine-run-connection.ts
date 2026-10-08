import type { CompanionBuildJob, CompanionStatus } from './api';

export function companionJobIsTerminal(job: Pick<CompanionBuildJob, 'status'>): boolean {
  return ['completed', 'failed', 'canceled', 'cancelled'].includes(job.status);
}

export class RefineRunConnection {
  readonly controller = new AbortController();
  jobId = '';
  accepted = false;
  connected = false;
  terminal = false;
  terminalStatus = '';
  detached = false;
  private disposed = false;
  private stopRequested = false;
  private cancelPending = false;

  constructor(private readonly actions: {
    cancel(jobId: string): Promise<CompanionStatus>;
    onStopping(stopping: boolean): void;
    onCancelled(job: CompanionBuildJob): void;
    onCancelError(cause: unknown): void;
  }) {}

  identify(jobId: string): boolean {
    if (!jobId || this.disposed || (this.jobId && this.jobId !== jobId)) return false;
    this.jobId = jobId;
    this.accepted = true;
    if (this.stopRequested) void this.cancel();
    return true;
  }

  requestStop(): void {
    if (this.disposed || this.terminal || this.cancelPending) return;
    this.stopRequested = true;
    this.actions.onStopping(true);
    if (this.jobId) void this.cancel();
  }

  complete(status = ''): void {
    this.terminal = true;
    if (status) this.terminalStatus = status;
    this.connected = false;
    if (!this.disposed) this.actions.onStopping(false);
  }

  disconnect(): void {
    this.detached = true;
    this.connected = false;
    this.controller.abort();
  }

  dispose(): void {
    this.disposed = true;
    this.disconnect();
  }

  private async cancel(): Promise<void> {
    if (this.cancelPending || this.disposed || this.terminal || !this.stopRequested || !this.jobId) return;
    this.cancelPending = true;
    try {
      const status = await this.actions.cancel(this.jobId);
      if (this.disposed || this.terminal) return;
      const job = status.jobs.find(item => item.id === this.jobId);
      if (!job || !companionJobIsTerminal(job)) throw new Error('Desktop has not confirmed that this build stopped. Check its progress before trying again.');
      this.complete(job.status);
      this.actions.onCancelled(job);
      this.controller.abort();
    } catch (cause) {
      if (!this.disposed && !this.terminal) this.actions.onCancelError(cause);
    } finally {
      this.cancelPending = false;
      this.stopRequested = false;
      if (!this.disposed) this.actions.onStopping(false);
    }
  }
}
