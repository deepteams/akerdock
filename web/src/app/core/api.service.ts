import { Injectable, computed, inject, signal } from '@angular/core';
import { Router } from '@angular/router';
import { AkerDockClient, ApiError } from '../../api/client';
import {
  credentialToJSON,
  toCreationOptions,
  toRequestOptions,
  webAuthnSupported,
} from './webauthn';

/**
 * Authentication is by SESSION COOKIE, not by a token the page holds.
 *
 * The dashboard used to ask for an API token and keep it in sessionStorage. That
 * works, but any XSS on this page could read it and walk away with a long-lived
 * credential. The session cookie is HttpOnly: JavaScript — ours or an
 * attacker's — cannot read it at all.
 *
 * The cost of cookies is CSRF, and we pay it explicitly: the server also sets a
 * readable akerdock_csrf cookie, and every mutation echoes it in a header. A page
 * on another origin can make the browser SEND our session cookie, but the
 * same-origin policy stops it from READING the CSRF one — so it cannot echo it.
 */
export interface CurrentUser {
  teamUuid: string;
  permissions: string[];
  /** The instance root (platform administrator, users.is_root) — gates the
   * global settings, outside the team-role model (rbac-matrix §3.5). */
  instanceRoot: boolean;
  email: string;
  name: string;
}

export interface Passkey {
  uuid: string;
  name: string;
  created_at: string;
  last_used_at: string | null;
}

/** Outcome of the password step: either a session, or "now the code". */
export interface SignInResult {
  mfaRequired: boolean;
  /** Echoed to verifyMfa with the TOTP code. Present when mfaRequired. */
  challenge?: string;
}

export interface MfaStatus {
  enabled: boolean;
  recovery_codes_remaining: number;
  confirmed_at?: string;
}

export interface TotpSetup {
  secret: string;
  otpauth_uri: string;
}

/** One sign-in button: the provider key and its display label. */
export interface OauthProviderButton {
  provider: string;
  name: string;
}

/** A federated identity linked to the account (security page). */
export interface LinkedIdentity {
  uuid: string;
  provider: string;
  email: string | null;
  created_at?: string;
}

@Injectable({ providedIn: 'root' })
export class ApiService {
  private readonly router = inject(Router);
  private readonly user = signal<CurrentUser | null>(null);
  private readonly csrf = signal<string | null>(null);

  readonly currentUser = this.user.asReadonly();
  readonly isAuthenticated = computed(() => this.user() !== null);

  readonly client = computed(
    () =>
      new AkerDockClient({
        baseUrl: '/api/v1',
        csrfToken: this.csrf() ?? undefined,
        onUnauthorized: () => this.handleUnauthorized(),
      }),
  );

  /**
   * A 401 from the API means the session expired mid-use: drop what we hold and
   * route to sign-in, so the operator sees the login page rather than a raw
   * "missing or invalid bearer token". Guarded against loops — once signed out
   * (or already on sign-in) it does nothing.
   */
  private handleUnauthorized(): void {
    if (this.user() === null) return;
    this.user.set(null);
    this.csrf.set(null);
    const returnUrl = this.router.url;
    void this.router.navigate(
      ['/sign-in'],
      returnUrl && returnUrl !== '/sign-in' ? { queryParams: { returnUrl } } : {},
    );
  }

  /** True when the current user's permissions include the given one. */
  can(permission: string): boolean {
    return this.user()?.permissions.includes(permission) ?? false;
  }

  /** Restores the session on boot: the cookie may still be valid from last time. */
  async restore(): Promise<boolean> {
    try {
      const res = await fetch('/auth/me', { credentials: 'same-origin' });
      if (!res.ok) return false;
      const body = (await res.json()) as {
        team_uuid: string;
        permissions: string[];
        instance_root?: boolean;
        csrf_token: string;
        email: string;
        name: string;
      };
      this.user.set({
        teamUuid: body.team_uuid,
        permissions: body.permissions,
        instanceRoot: body.instance_root ?? false,
        email: body.email,
        name: body.name,
      });
      this.csrf.set(body.csrf_token);
      return true;
    } catch {
      return false;
    }
  }

  async signIn(email: string, password: string): Promise<SignInResult> {
    const res = await fetch('/auth/login', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email, password }),
    });
    if (!res.ok) {
      const body = (await res.json().catch(() => ({}))) as { message?: string };
      throw new Error(body.message ?? 'Sign-in failed');
    }
    const body = (await res.json()) as {
      csrf_token?: string;
      mfa_required?: boolean;
      challenge?: string;
    };
    if (body.mfa_required) {
      // The password was right but no session exists yet: the caller must
      // come back through verifyMfa with the code from the authenticator.
      return { mfaRequired: true, challenge: body.challenge };
    }
    this.csrf.set(body.csrf_token ?? null);
    await this.restore();
    return { mfaRequired: false };
  }

  /** Step two of a two-step sign-in: the challenge from signIn plus a TOTP
   *  code — or a recovery code, when the authenticator is gone. */
  async verifyMfa(challenge: string, code: string, recoveryCode = ''): Promise<void> {
    const body = await this.authPost<{ csrf_token: string }>('/auth/mfa/verify', {
      challenge,
      code,
      recovery_code: recoveryCode,
    });
    this.csrf.set(body.csrf_token);
    await this.restore();
  }

  async mfaStatus(): Promise<MfaStatus> {
    const res = await fetch('/auth/mfa', { credentials: 'same-origin' });
    if (!res.ok) throw await this.authError(res);
    return (await res.json()) as MfaStatus;
  }

  /** Starts TOTP enrolment: the secret for the authenticator app. Nothing is
   *  enforced until confirmTotp proves the app actually holds it. */
  async setupTotp(): Promise<TotpSetup> {
    return this.authPost<TotpSetup>('/auth/mfa/totp/setup', {});
  }

  /** Confirms enrolment with a first code; returns the recovery codes —
   *  shown ONCE, stored hashed, never retrievable again. */
  async confirmTotp(code: string): Promise<string[]> {
    const body = await this.authPost<{ recovery_codes: string[] }>('/auth/mfa/totp/confirm', {
      code,
    });
    return body.recovery_codes;
  }

  /** Disabling 2FA requires proving it: a valid code or a recovery code. */
  async disableTotp(code: string, recoveryCode = ''): Promise<void> {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' };
    if (this.csrf()) headers['X-CSRF-Token'] = this.csrf()!;
    const res = await fetch('/auth/mfa/totp', {
      method: 'DELETE',
      credentials: 'same-origin',
      headers,
      body: JSON.stringify({ code, recovery_code: recoveryCode }),
    });
    if (!res.ok && res.status !== 204) throw await this.authError(res);
  }

  async regenerateRecoveryCodes(code: string): Promise<string[]> {
    const body = await this.authPost<{ recovery_codes: string[] }>('/auth/mfa/recovery-codes', {
      code,
    });
    return body.recovery_codes;
  }

  /** Whether this browser can do WebAuthn at all. */
  passkeysSupported(): boolean {
    return webAuthnSupported();
  }

  /**
   * Usernameless passkey sign-in: the authenticator names the user, the
   * signature proves them, and phishing gets nothing — the signature binds the
   * origin, so a look-alike domain cannot replay it.
   */
  async signInWithPasskey(): Promise<void> {
    const begin = await this.authPost<{ ceremony: string; options: never }>(
      '/auth/passkey/login/begin',
      {},
    );
    const credential = (await navigator.credentials.get(
      toRequestOptions(begin.options),
    )) as PublicKeyCredential | null;
    if (!credential) throw new Error('Passkey sign-in was cancelled');

    const body = await this.authPost<{ csrf_token: string }>('/auth/passkey/login/finish', {
      ceremony: begin.ceremony,
      credential: credentialToJSON(credential),
    });
    this.csrf.set(body.csrf_token);
    await this.restore();
  }

  /**
   * Passkey step-up (rbac-matrix §5): proves the signed-in user still holds
   * their passkey before a sensitive action — today, opening a server
   * terminal. Stamps the session server-side for a few minutes.
   */
  async stepUpWithPasskey(): Promise<void> {
    const begin = await this.authPost<{ ceremony: string; options: never }>(
      '/auth/passkey/stepup/begin',
      {},
    );
    const credential = (await navigator.credentials.get(
      toRequestOptions(begin.options),
    )) as PublicKeyCredential | null;
    if (!credential) throw new Error('Passkey step-up was cancelled');

    await this.authPost('/auth/passkey/stepup/finish', {
      ceremony: begin.ceremony,
      credential: credentialToJSON(credential),
    });
  }

  /** Enrols a new passkey for the signed-in user. */
  async registerPasskey(name: string): Promise<Passkey> {
    const begin = await this.authPost<{ ceremony: string; options: never }>(
      '/auth/passkeys/register/begin',
      {},
    );
    const credential = (await navigator.credentials.create(
      toCreationOptions(begin.options),
    )) as PublicKeyCredential | null;
    if (!credential) throw new Error('Passkey enrolment was cancelled');

    return this.authPost<Passkey>('/auth/passkeys/register/finish', {
      ceremony: begin.ceremony,
      name,
      credential: credentialToJSON(credential),
    });
  }

  async listPasskeys(): Promise<Passkey[]> {
    const res = await fetch('/auth/passkeys', { credentials: 'same-origin' });
    if (!res.ok) throw await this.authError(res);
    const body = (await res.json()) as { data: Passkey[] };
    return body.data;
  }

  async deletePasskey(uuid: string): Promise<void> {
    const res = await fetch(`/auth/passkeys/${encodeURIComponent(uuid)}`, {
      method: 'DELETE',
      credentials: 'same-origin',
      headers: this.csrf() ? { 'X-CSRF-Token': this.csrf()! } : {},
    });
    if (!res.ok && res.status !== 204) throw await this.authError(res);
  }

  /** The OAuth/OIDC providers the sign-in page can offer. Anonymous. */
  async oauthProviders(): Promise<OauthProviderButton[]> {
    const res = await fetch('/auth/oauth/providers', { credentials: 'same-origin' });
    if (!res.ok) return [];
    const body = (await res.json()) as { data: OauthProviderButton[] };
    return body.data;
  }

  /**
   * Starts an OAuth round-trip and NAVIGATES AWAY: the server mints the
   * state and answers the provider's authorize URL; the browser leaves for
   * the IdP and comes back on /auth/oauth/{provider}/callback — signed in,
   * or on /sign-in with an error code.
   */
  async startOauth(provider: string, purpose: 'login' | 'link' = 'login'): Promise<void> {
    const path =
      `/auth/oauth/${encodeURIComponent(provider)}/start` +
      (purpose === 'link' ? '?purpose=link' : '');
    const body = await this.authPost<{ url: string }>(path, {});
    window.location.href = body.url;
  }

  async listIdentities(): Promise<LinkedIdentity[]> {
    const res = await fetch('/auth/identities', { credentials: 'same-origin' });
    if (!res.ok) throw await this.authError(res);
    const body = (await res.json()) as { data: LinkedIdentity[] };
    return body.data;
  }

  async deleteIdentity(uuid: string): Promise<void> {
    const res = await fetch(`/auth/identities/${encodeURIComponent(uuid)}`, {
      method: 'DELETE',
      credentials: 'same-origin',
      headers: this.csrf() ? { 'X-CSRF-Token': this.csrf()! } : {},
    });
    if (!res.ok && res.status !== 204) throw await this.authError(res);
  }

  /** POST to an /auth endpoint: session cookie rides along, CSRF echoed. */
  private async authPost<T>(path: string, body: unknown): Promise<T> {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' };
    if (this.csrf()) headers['X-CSRF-Token'] = this.csrf()!;
    const res = await fetch(path, {
      method: 'POST',
      credentials: 'same-origin',
      headers,
      body: JSON.stringify(body),
    });
    if (!res.ok) throw await this.authError(res);
    return (await res.json()) as T;
  }

  private async authError(res: Response): Promise<Error> {
    const body = (await res.json().catch(() => ({}))) as { message?: string; code?: string };
    return new Error(body.message ?? `Request failed (${res.status})`);
  }

  /** Logging out revokes the session on the SERVER: dropping the cookie alone
   *  would leave a valid session behind, which is not a logout. */
  async signOut(): Promise<void> {
    await fetch('/auth/logout', {
      method: 'POST',
      credentials: 'same-origin',
      headers: this.csrf() ? { 'X-CSRF-Token': this.csrf()! } : {},
    }).catch(() => undefined);
    this.user.set(null);
    this.csrf.set(null);
  }

  static describe(error: unknown): string {
    if (error instanceof ApiError) {
      const details = error.details.map((d) => d.message).filter(Boolean);
      return details.length ? `${error.message}: ${details.join(', ')}` : error.message;
    }
    return error instanceof Error ? error.message : 'Unexpected error';
  }
}
