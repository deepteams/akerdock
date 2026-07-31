import { Injectable, signal } from '@angular/core';

export interface ConfirmRequest {
  title: string;
  /** The consequence, named concretely — "Delete the application my-api?" */
  message: string;
  /** Label of the confirming button. Defaults to "Confirm". */
  confirmLabel?: string;
  /** Destructive styling (danger-tinted modal). Defaults to true. */
  danger?: boolean;
}

interface PendingConfirm extends Required<ConfirmRequest> {
  resolve: (answer: boolean) => void;
}

/**
 * In-app replacement for window.confirm(): one promise-returning ask(),
 * rendered by <akd-confirm-host> (mounted once, in the shell) over
 * <akd-modal danger> — themed, named, and non-thread-blocking, unlike the
 * OS-chrome alert box.
 */
@Injectable({ providedIn: 'root' })
export class ConfirmService {
  readonly pending = signal<PendingConfirm | null>(null);

  ask(request: ConfirmRequest): Promise<boolean> {
    // A second ask while one is open answers the first with "no" — the caller
    // that navigated away or re-triggered must not stay suspended forever.
    this.pending()?.resolve(false);
    return new Promise((resolve) =>
      this.pending.set({ confirmLabel: 'Confirm', danger: true, ...request, resolve }),
    );
  }

  settle(answer: boolean): void {
    const pending = this.pending();
    if (!pending) return;
    this.pending.set(null);
    pending.resolve(answer);
  }
}
