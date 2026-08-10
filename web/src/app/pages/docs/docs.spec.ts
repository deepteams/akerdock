import { TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';

import { DocsArticleComponent } from './docs-article.component';
import {
  groupTopics,
  isBeyondRole,
  searchTopics,
  sectionAnchor,
  type DocSection,
  type DocTopic,
} from './docs.model';

// The manual itself is no longer here to be tested: the corpus is Markdown
// under internal/docs/manual/, and its invariants — unique ids, icons that
// exist, links that resolve, permissions in the catalogue, a manual that still
// covers a plain member's day — are Go tests beside it (ADR-072). What is left
// on this side is what this side owns: search, grouping, and the rendering of
// the HTML the API returns.

function section(over: Partial<DocSection> = {}): DocSection {
  return { id: 's', title: 'S', html: '<p>body</p>', text: 'body', ...over };
}

function topic(over: Partial<DocTopic> = {}): DocTopic {
  return {
    id: 't',
    title: 'T',
    group: 'G',
    summary: 'a summary long enough to read',
    icon: 'rocket',
    sections: [section()],
    ...over,
  };
}

describe('searchTopics', () => {
  const corpus: DocTopic[] = [
    topic({
      id: 'previews',
      title: 'Pull request previews',
      summary: 'One live instance per PR.',
      sections: [
        section({ id: 'forks', title: 'From a fork', text: 'A fork PR is not deployed', html: '' }),
      ],
    }),
    topic({
      id: 'logs',
      title: 'Logs',
      summary: 'Reading what a container printed.',
      group: 'Run and debug',
      sections: [section({ text: 'the log stream resumes where it stopped' })],
    }),
  ];

  it('requires every term, and looks inside the sections', () => {
    expect(searchTopics(corpus, 'fork previews').map((t) => t.id)).toEqual(['previews']);
    expect(searchTopics(corpus, 'resumes').map((t) => t.id)).toEqual(['logs']);
    expect(searchTopics(corpus, 'fork resumes')).toEqual([]);
  });

  it('matches titles, summaries and group names as well as the prose', () => {
    expect(searchTopics(corpus, 'container').map((t) => t.id)).toEqual(['logs']);
    expect(searchTopics(corpus, 'debug').map((t) => t.id)).toEqual(['logs']);
    expect(searchTopics(corpus, 'PULL Request').map((t) => t.id)).toEqual(['previews']);
  });

  // The API hands the searchable prose beside the markup precisely so a query
  // never has to be matched against tags: "code" must find a paragraph about
  // code, not every topic whose HTML happens to contain a <code> element.
  it('searches the plain text, never the markup', () => {
    const marked = [
      topic({ id: 'marked', sections: [section({ html: '<p><code>x</code></p>', text: 'x' })] }),
    ];
    expect(searchTopics(marked, 'code')).toEqual([]);
  });

  it('reads the intro prose the topic may carry before its first section', () => {
    const withIntro = [
      topic({ id: 'intro', intro_html: '<p>before</p>', intro_text: 'ambient prologue' }),
    ];
    expect(searchTopics(withIntro, 'prologue').map((t) => t.id)).toEqual(['intro']);
  });

  it('returns everything on an empty query, in the order it was given', () => {
    expect(searchTopics(corpus, '   ').map((t) => t.id)).toEqual(['previews', 'logs']);
  });

  it('does not mutate the topics it filters', () => {
    searchTopics(corpus, 'fork');
    expect(corpus.length).toBe(2);
    expect(corpus[0].sections.length).toBe(1);
  });
});

describe('groupTopics', () => {
  // The API returns the manual in READING order — group by group, and in the
  // author's order inside each group. Sorting anything here would silently
  // reshuffle the manual, so the grouping only ever preserves.
  it('keeps each group in order of first appearance, and each topic in its own', () => {
    const topics = [
      topic({ id: 'a', group: 'Start here' }),
      topic({ id: 'b', group: 'Ship code' }),
      topic({ id: 'c', group: 'Start here' }),
      topic({ id: 'd', group: 'Ship code' }),
    ];
    const groups = groupTopics(topics);
    expect(groups.map((g) => g.title)).toEqual(['Start here', 'Ship code']);
    expect(groups[0].topics.map((t) => t.id)).toEqual(['a', 'c']);
    expect(groups[1].topics.map((t) => t.id)).toEqual(['b', 'd']);
  });

  it('loses no topic', () => {
    const topics = [topic({ id: 'a' }), topic({ id: 'b', group: 'H' }), topic({ id: 'c' })];
    expect(groupTopics(topics).reduce((n, g) => n + g.topics.length, 0)).toBe(3);
  });

  it('groups an empty manual into no groups at all', () => {
    expect(groupTopics([])).toEqual([]);
  });
});

describe('isBeyondRole', () => {
  // The flag is the SERVER's verdict (ADR-072 §4): without ?all=true a topic
  // past the reader's role is simply absent, and the client never recomputes
  // the predicate — it only marks what came back marked.
  it('is true only for what the server flagged', () => {
    expect(isBeyondRole(topic({ beyond_role: true }))).toBeTrue();
    expect(isBeyondRole(topic())).toBeFalse();
    expect(isBeyondRole(topic({ permission: 'instance:manage', root: true }))).toBeFalse();
  });
});

describe('sectionAnchor', () => {
  it('namespaces the anchor with its topic — a section id is unique inside one', () => {
    expect(sectionAnchor('previews', 'day-to-day')).toBe('previews--day-to-day');
    expect(sectionAnchor('logs', 'day-to-day')).not.toBe(sectionAnchor('previews', 'day-to-day'));
  });
});

describe('the article renderer', () => {
  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideRouter([])] });
  });

  function render(t: DocTopic): HTMLElement {
    const fixture = TestBed.createComponent(DocsArticleComponent);
    fixture.componentRef.setInput('topic', t);
    fixture.detectChanges();
    return fixture.nativeElement as HTMLElement;
  }

  it('renders the HTML the API returned, as HTML', () => {
    const el = render(
      topic({
        sections: [
          section({
            html: '<h3>Deeper</h3><p>A <code>flag</code> and a <strong>word</strong>.</p><ul><li>one</li></ul>',
            text: 'Deeper A flag and a word. one',
          }),
        ],
      }),
    );
    const prose = el.querySelector('.akd-prose')!;
    expect(prose.querySelector('h3')?.textContent).toBe('Deeper');
    expect(prose.querySelector('code')?.textContent).toBe('flag');
    expect(prose.querySelector('strong')?.textContent).toBe('word');
    expect(prose.querySelectorAll('li').length).toBe(1);
  });

  // THE test of ADR-072 §3's second half. The server's markup comes from a
  // Markdown parser with raw HTML disabled, and [innerHTML] runs Angular's
  // DomSanitizer over it anyway. Bind it any other way — through one of
  // DomSanitizer's trust-me escape hatches, or by assigning innerHTML by hand
  // — and this script survives.
  it('sanitises what it binds: a script never reaches the document', () => {
    const el = render(
      topic({
        sections: [
          section({
            html: '<p>before</p><script>window.__docsPwned = true;</script><img src="data:," onerror="window.__docsPwned = true">',
            text: 'before',
          }),
        ],
      }),
    );
    const prose = el.querySelector('.akd-prose')!;
    expect(prose.querySelector('script')).toBeNull();
    expect(prose.innerHTML).not.toContain('onerror');
    expect(prose.textContent).toContain('before');
    expect((globalThis as Record<string, unknown>)['__docsPwned']).toBeUndefined();
  });

  it('sanitises the intro prose on the same terms', () => {
    const el = render(topic({ intro_html: '<p>lede</p><script>void 0;</script>' }));
    const intro = el.querySelector('.intro')!;
    expect(intro.textContent).toContain('lede');
    expect(intro.querySelector('script')).toBeNull();
  });

  // A grep over the sources is what a reviewer runs; from inside the browser
  // the reachable equivalent is the compiled component itself. The weaker of
  // the two nets — the one above is the strong one, because it fails on the
  // BEHAVIOUR rather than on the spelling. The name is assembled rather than
  // written so that the repository-wide grep finds no occurrence at all, not
  // even the one that guards against it.
  it('names none of DomSanitizer’s trust-me escape hatches', () => {
    const forbidden = ['bypass', 'Security', 'Trust'].join('');
    const compiled = DocsArticleComponent as unknown as { ɵcmp?: { template?: unknown } };
    const source = [String(DocsArticleComponent), String(compiled.ɵcmp?.template ?? '')].join('\n');
    expect(source).not.toContain(forbidden);
  });

  it('lists a table of contents only when there is more than one section', () => {
    const one = render(topic());
    expect(one.querySelector('.toc')).toBeNull();

    const many = render(
      topic({ sections: [section({ id: 'a', title: 'A' }), section({ id: 'b', title: 'B' })] }),
    );
    const links = Array.from(many.querySelectorAll('.toc a'));
    expect(links.map((a) => a.textContent?.trim())).toEqual(['A', 'B']);
    // Anchors are namespaced by topic, and match the section elements' ids.
    const ids = Array.from(many.querySelectorAll('section.section')).map((s) => s.id);
    expect(ids).toEqual(['t--a', 't--b']);
    expect(links.map((a) => a.getAttribute('href'))).toEqual(ids.map((id) => `#${id}`));
  });

  it('marks a topic the reader only sees because they asked for the whole manual', () => {
    expect(render(topic()).querySelector('.beyond')).toBeNull();
    expect(render(topic({ beyond_role: true })).querySelector('.beyond')).not.toBeNull();
  });

  it('marks a section that is beyond the role inside a topic that is not', () => {
    const el = render(
      topic({
        sections: [
          section({ id: 'a', title: 'A' }),
          section({ id: 'b', title: 'B', beyond_role: true }),
        ],
      }),
    );
    const badges = Array.from(el.querySelectorAll('section.section .akd-badge'));
    expect(badges.length).toBe(1);
    expect(badges[0].closest('section')?.id).toBe('t--b');
  });

  it('renders the links out of the manual — a route inside, a URL out', () => {
    const el = render(
      topic({
        links: [
          { label: 'Applications', route: '/applications' },
          { label: 'The CLI', href: 'https://example.com/cli' },
        ],
      }),
    );
    const anchors = Array.from(el.querySelectorAll('.links a'));
    expect(anchors.map((a) => a.getAttribute('href'))).toEqual([
      '/applications',
      'https://example.com/cli',
    ]);
    expect(anchors[1].getAttribute('rel')).toBe('noopener noreferrer');
  });

  it('falls back to a generic icon for a topic whose front-matter names none', () => {
    const el = render(topic({ icon: undefined }));
    expect(el.querySelector('h1 akd-icon svg')).not.toBeNull();
  });

  it('says so rather than drawing a blank page when every section was filtered out', () => {
    const el = render(topic({ sections: [] }));
    expect(el.querySelector('.akd-muted')?.textContent).toContain('no content');
  });
});
