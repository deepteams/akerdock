import { Injectable, signal } from '@angular/core';

export interface ConfirmRequest {
  title: string;
  /** The consequence, named concretely — "Delete the application my-api?" */
  message: string;
  /** Label of the confirming button. Defaults to "Confirm". */
  confirmLabel?: string;
  /** Destructive styling (danger-tinted modal). Defaults to true. */
  danger?: boolean;
  /**
   * A second way forward, when the question genuinely has two answers that
   * are not each other's opposite — "stop them and start this one" beside
   * "start it alongside them". Absent for the ordinary yes/no.
   */
  alternativeLabel?: string;
  /** Extra lines under the message: the arithmetic behind the question. */
  bullets?: string[];
}

/** What the operator chose. `ask()` collapses this to a boolean. */
export type ConfirmChoice = 'confirm' | 'alternative' | 'cancel';

interface PendingConfirm extends Required<Omit<ConfirmRequest, 'alternativeLabel' | 'bullets'>> {
  alternativeLabel: string | null;
  bullets: string[];
  resolve: (answer: ConfirmChoice) => void;
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

  /** The ordinary yes/no. */
  async ask(request: ConfirmRequest): Promise<boolean> {
    return (await this.askChoice(request)) === 'confirm';
  }

  /** The three-way ask, for a question whose two answers are both a way
   * forward. Without an alternativeLabel it never resolves to 'alternative'. */
  askChoice(request: ConfirmRequest): Promise<ConfirmChoice> {
    // A second ask while one is open answers the first with "no" — the caller
    // that navigated away or re-triggered must not stay suspended forever.
    this.pending()?.resolve('cancel');
    return new Promise((resolve) =>
      this.pending.set({
        confirmLabel: 'Confirm',
        danger: true,
        ...request,
        alternativeLabel: request.alternativeLabel ?? null,
        bullets: request.bullets ?? [],
        resolve,
      }),
    );
  }

  settle(answer: boolean): void {
    this.settleChoice(answer ? 'confirm' : 'cancel');
  }

  settleChoice(answer: ConfirmChoice): void {
    const pending = this.pending();
    if (!pending) return;
    this.pending.set(null);
    pending.resolve(answer);
  }
}
