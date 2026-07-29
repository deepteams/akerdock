import { Injectable, inject, signal } from '@angular/core';
import { NavigationEnd, Router, UrlTree } from '@angular/router';
import { filter } from 'rxjs';

/**
 * Remembers the last page visited, so a "back" arrow can lead where the user
 * actually came from.
 *
 * A resource is reachable by more than one route — an application from the flat
 * /applications list AND from its environment's resource table. A back arrow
 * hard-coded to the list therefore lands a third of the users somewhere they
 * have never been, which reads as the app losing their place.
 *
 * Two rules make this behave:
 *
 *   - **Query-parameter-only changes are not navigations.** Detail pages carry
 *     their open tab in `?tab=`, and each tab switch pushes a history entry.
 *     Going "back" to the previous tab of the page one is looking at is not
 *     what an arrow at the top of that page promises.
 *   - **The fallback is used until there IS a previous page.** On a page opened
 *     from a bookmark or a full reload there is nothing behind us in this app,
 *     and `history.back()` would leave it entirely.
 *
 * It must be constructed BEFORE the first navigation it is expected to know
 * about — a service instantiated by the page that asks the question has by
 * definition missed how the user got there. The shell injects it for that
 * reason alone.
 */
@Injectable({ providedIn: 'root' })
export class NavigationHistory {
  private readonly router = inject(Router);
  /** A signal, not a field: the back arrow is rendered by OnPush components
   *  whose template may be evaluated before the NavigationEnd that brought
   *  them there — reading a signal makes the link correct itself. */
  private readonly previousUrl = signal<string | null>(null);
  private currentUrl: string | null = null;

  constructor() {
    this.router.events
      .pipe(filter((event): event is NavigationEnd => event instanceof NavigationEnd))
      .subscribe((event) => {
        const url = event.urlAfterRedirects;
        if (path(url) === path(this.currentUrl)) {
          this.currentUrl = url; // same page, other tab: keep the newest form
          return;
        }
        this.previousUrl.set(this.currentUrl);
        this.currentUrl = url;
      });
  }

  /**
   * Where a back arrow should lead: the previous page, with its query string
   * intact (a list keeps its cursor and filters), or `fallback` when this page
   * is the first one of the session.
   *
   * Returns a UrlTree so `[routerLink]` renders a real href — middle-click and
   * "open in new tab" keep working, which `history.back()` cannot offer.
   */
  backTo(fallback: string): UrlTree {
    return this.router.parseUrl(this.previousUrl() ?? fallback);
  }
}

function path(url: string | null): string | null {
  return url === null ? null : url.split('?')[0];
}
