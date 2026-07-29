import { TestBed } from '@angular/core/testing';
import { NavigationEnd, Router } from '@angular/router';
import { Subject } from 'rxjs';
import { NavigationHistory } from './navigation-history.service';

// The back arrow of a detail page is the only navigation whose target is not
// written anywhere: it depends on where the user came from. These tests pin
// that behaviour, because getting it wrong is invisible in code review and
// obvious to whoever clicks it.

class RouterStub {
  readonly events = new Subject<NavigationEnd>();
  parseUrl(url: string): unknown {
    return url; // the tree is opaque here; the URL is what we assert on
  }
  go(url: string): void {
    this.events.next(new NavigationEnd(1, url, url));
  }
}

/** The stub's parseUrl echoes the URL, so assertions read as plain strings. */
function backOf(history: NavigationHistory, fallback: string): string {
  return history.backTo(fallback) as unknown as string;
}

function historyWith(...urls: string[]): { history: NavigationHistory; router: RouterStub } {
  const router = new RouterStub();
  TestBed.configureTestingModule({
    providers: [NavigationHistory, { provide: Router, useValue: router }],
  });
  const history = TestBed.inject(NavigationHistory);
  urls.forEach((url) => router.go(url));
  return { history, router };
}

describe('NavigationHistory', () => {
  afterEach(() => TestBed.resetTestingModule());

  it('falls back when the page is the first of the session', () => {
    const { history } = historyWith('/applications/abc');
    expect(backOf(history, '/applications')).toBe('/applications');
  });

  it('returns to the page the user actually came from', () => {
    // The case that made the arrow feel broken: reached from an environment,
    // the arrow used to land on the flat /applications list.
    const { history } = historyWith('/projects/p1/environments/e1', '/applications/abc');
    expect(backOf(history, '/applications')).toBe('/projects/p1/environments/e1');
  });

  it('keeps the previous page query string, so a list keeps its place', () => {
    const { history } = historyWith('/applications?cursor=42', '/applications/abc');
    expect(backOf(history, '/applications')).toBe('/applications?cursor=42');
  });

  it('does not treat a tab switch as a navigation', () => {
    // Tabs live in ?tab= and each one pushes a history entry. Going "back" to
    // the previous tab of the page one is looking at is not what the arrow at
    // the top of that page promises.
    const { history } = historyWith(
      '/projects/p1/environments/e1',
      '/applications/abc',
      '/applications/abc?tab=logs',
      '/applications/abc?tab=envs',
    );
    expect(backOf(history, '/applications')).toBe('/projects/p1/environments/e1');
  });

  it('follows the user across several pages', () => {
    const { history } = historyWith('/applications', '/applications/abc', '/servers');
    expect(backOf(history, '/servers')).toBe('/applications/abc');
  });
});
