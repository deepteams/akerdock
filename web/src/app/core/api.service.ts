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
  /** The instance requires MFA and this user has no confirmed factor yet: the
   * app is blocked until they enrol one (forced enrollment). */
  mfaEnrollmentRequired: boolean;
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

/** What an invitation link says about itself, before anyone signs in. */
export interface InvitationInfo {
  email: string;
  team_name: string;
  role: string;
  expires_at: string;
  /** True when this address already has an account: joining is then a sign-in,
   *  not a sign-up. */
  account_exists: boolean;
  /** SSO-only instance: no password account can be created here. */
  password_login_disabled: boolean;
}

/** Outcome of redeeming an invitation link. */
export interface JoinedTeam {
  team_uuid: string;
  name: string;
  /** True when the session now acts in that team (the normal case): the caller
   *  must reload, because everything on screen belongs to the previous one. */
  switched: boolean;
}

/** One team the signed-in user is a member of — an entry of the switcher. */
export interface MyTeam {
  uuid: string;
  name: string;
  role: string;
  personal: boolean;
  /** True for the team the session is acting in right now. */
  current: boolean;
}

/** Public metadata of a pending CLI login request (ADR-031 consent page). */
export interface CliAuthRequestInfo {
  user_code: string;
  name: string;
  status: string;
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
  private readonly passwordDisabled = signal(false);

  readonly currentUser = this.user.asReadonly();
  /** SSO-only mode: the sign-in page hides the password form. Set by
   * oauthProviders(). */
  readonly passwordLoginDisabled = this.passwordDisabled.asReadonly();
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

  /**
   * True when the current user's permissions include the given one. The
   * team-scoped `root` permission implies every other one (rbac-matrix §2),
   * so a root user is never shown a degraded UI.
   */
  can(permission: string): boolean {
    const permissions = this.user()?.permissions;
    if (!permissions) return false;
    return permissions.includes('root') || permissions.includes(permission);
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
        mfa_enrollment_required?: boolean;
      };
      this.user.set({
        teamUuid: body.team_uuid,
        permissions: body.permissions,
        instanceRoot: body.instance_root ?? false,
        email: body.email,
        name: body.name,
        mfaEnrollmentRequired: body.mfa_enrollment_required ?? false,
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

  /**
   * TOTP step-up (ADR-045 §5) — the counterpart of the passkey ceremony for
   * users whose second factor is a TOTP. The server decides WHICH factor a
   * given user must present, so the UI never offers a choice: it asks for the
   * one named in the refusal.
   *
   * Recovery codes are accepted here for the same reason they are at login:
   * someone who lost their phone still has to reach their production database.
   */
  async stepUpWithTotp(code: string, recoveryCode?: string): Promise<void> {
    await this.authPost('/auth/mfa/totp/stepup', {
      code: code || undefined,
      recovery_code: recoveryCode || undefined,
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
    const body = (await res.json()) as {
      data: OauthProviderButton[];
      password_login_disabled?: boolean;
    };
    this.passwordDisabled.set(body.password_login_disabled ?? false);
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

  /**
   * Redeems an invitation link for the signed-in user (ADR-038): joins the team
   * the invitation names. Requires an active session — the invitation email must
   * match the current account, enforced server-side.
   */
  async acceptInvitation(token: string): Promise<JoinedTeam> {
    const joined = await this.authPost<JoinedTeam>('/auth/invitations/accept', { token });
    // Accepting moves the session into the team just joined, so the permissions
    // this account holds have changed with it.
    if (joined.switched) await this.restore();
    return joined;
  }

  /**
   * What an invitation link is for, before anyone signs in. A POST for a read:
   * the token is a credential and has no business in a URL that proxies log.
   */
  async invitationInfo(token: string): Promise<InvitationInfo> {
    return this.authPost<InvitationInfo>('/auth/invitations/lookup', { token });
  }

  /**
   * Creates the invitee's account from the link and opens their session. The
   * email is NOT sent: it comes from the invitation, server-side — an invitee
   * does not get to choose which address they register.
   */
  async signUpFromInvitation(token: string, name: string, password: string): Promise<void> {
    const body = await this.authPost<{ csrf_token: string }>('/auth/invitations/signup', {
      token,
      name,
      password,
    });
    this.csrf.set(body.csrf_token ?? null);
    await this.restore();
  }

  /**
   * The teams the signed-in user may act in (PRD §37). Not GET /teams, which
   * lists the whole instance for the root: the switcher must only ever offer
   * memberships.
   */
  async myTeams(): Promise<MyTeam[]> {
    const res = await fetch('/auth/teams', { credentials: 'same-origin' });
    if (!res.ok) throw await this.authError(res);
    const body = (await res.json()) as { data: MyTeam[] };
    return body.data;
  }

  /**
   * Moves the session into another team. The server is the one that decides
   * membership; on success the local user is refreshed, because the team change
   * also changes the permissions this account holds — a member of one team can
   * be a reviewer in the next.
   */
  async switchTeam(teamUuid: string): Promise<void> {
    await this.authPost<{ team_uuid: string }>('/auth/session/team', { team_uuid: teamUuid });
    await this.restore();
  }

  /** What a pending CLI login request says about itself (consent page). */
  async cliAuthRequest(requestId: string): Promise<CliAuthRequestInfo> {
    const res = await fetch('/auth/cli/request?request_id=' + encodeURIComponent(requestId), {
      credentials: 'same-origin',
    });
    if (!res.ok) throw await this.authError(res);
    return (await res.json()) as CliAuthRequestInfo;
  }

  /**
   * The maintainer's explicit consent to a CLI login request. The server
   * narrows the grant to the session's own permissions and answers 204.
   */
  async approveCliAuth(requestId: string, teamUuid: string, permissions: string[]): Promise<void> {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' };
    if (this.csrf()) headers['X-CSRF-Token'] = this.csrf()!;
    const res = await fetch('/auth/cli/approve', {
      method: 'POST',
      credentials: 'same-origin',
      headers,
      body: JSON.stringify({ request_id: requestId, team_uuid: teamUuid, permissions }),
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
