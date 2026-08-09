import { ICONS } from '../../../ui/icon/icons';
import { DOC_TOPICS } from './docs.content';
import { groupTopics, inlineRuns, narrowTopics, searchTopics, type DocTopic } from './docs.model';

/** The granular permission catalogue (rbac-matrix §1.2 / internal/auth). A gate
 *  naming anything else would silently hide a page from everybody, since
 *  `can()` answers false for an unknown permission. */
const CATALOGUE = new Set([
  'applications:create', 'applications:delete', 'applications:deploy', 'applications:exec',
  'applications:lifecycle', 'applications:read', 'applications:update', 'audit:read',
  'backups:manage', 'backups:read', 'backups:restore', 'certificates:read', 'certificates:renew',
  'cloud:manage', 'cloud:read', 'config:apply', 'config:export', 'databases:create',
  'databases:credentials', 'databases:delete', 'databases:lifecycle', 'databases:read',
  'databases:update', 'deployments:cancel', 'deployments:read', 'environments:deploy',
  'environments:manage', 'environments:read', 'external-endpoints:manage',
  'external-endpoints:read', 'ingress-endpoints:manage', 'ingress-endpoints:read',
  'ingress-tunnels:open', 'instance:audit', 'instance:encryption', 'instance:manage',
  'invitations:manage', 'jobs:manage', 'keys:manage', 'keys:read', 'keys:reveal', 'logs:manage',
  'logs:read', 'members:manage', 'members:read', 'metrics:read', 'notifications:manage',
  'notifications:read', 'port-forwards:open', 'previews:manage', 'previews:read',
  'projects:manage', 'projects:read', 'registries:manage', 'resources:adopt', 'resources:read',
  'roles:manage', 'roles:read', 'secrets:read', 'secrets:reveal', 'secrets:write',
  'servers:maintain', 'servers:manage', 'servers:proxy', 'servers:read', 'services:deploy',
  'services:manage', 'services:read', 'sources:manage', 'sources:read', 'storages:manage',
  'team:manage', 'team:read', 'templates:manage', 'terminal:open', 'terminal:root',
  'tokens:create', 'tokens:read', 'tokens:revoke', 'uptime:manage', 'uptime:read',
]);

/** The `member` role — the reader this manual is written for (internal/auth/roles.go). */
const MEMBER = new Set([
  'team:read', 'members:read', 'projects:read', 'projects:manage', 'environments:read',
  'environments:manage', 'resources:read', 'resources:adopt', 'applications:read',
  'applications:create', 'applications:update', 'applications:delete', 'applications:deploy',
  'applications:lifecycle', 'applications:exec', 'databases:read', 'databases:create',
  'databases:update', 'databases:delete', 'databases:lifecycle', 'services:read',
  'services:manage', 'services:deploy', 'secrets:read', 'secrets:write', 'servers:read',
  'certificates:read', 'keys:read', 'sources:read', 'sources:manage', 'registries:manage',
  'storages:manage', 'backups:read', 'backups:manage', 'backups:restore', 'deployments:read',
  'deployments:cancel', 'previews:read', 'previews:manage', 'terminal:open',
  'port-forwards:open', 'external-endpoints:read', 'ingress-endpoints:read',
  'ingress-tunnels:open', 'logs:read', 'metrics:read', 'notifications:read',
  'notifications:manage', 'uptime:read', 'uptime:manage', 'audit:read',
]);

/** First URL segment of every route declared under the shell (app.routes.ts).
 *  Written out rather than imported: pulling app.routes in drags the router
 *  guard, the API service and half the app into this unit test. */
const ROUTE_SEGMENTS = new Set([
  'projects', 'applications', 'services', 'databases', 'servers', 'jobs', 'events',
  'notifications', 'sources', 'github-apps', 'private-keys', 'registries', 'dns-credentials',
  's3-storages', 'external-endpoints', 'ingress', 'docs', 'team', 'settings', 'system',
  'security',
]);

const can = (granted: Set<string>) => (permission: string) => granted.has(permission);
const nobody = () => false;
const everybody = () => true;

describe('inlineRuns', () => {
  it('splits code and strong spans out of plain text', () => {
    expect(inlineRuns('run `akerdock logs` now')).toEqual([
      { text: 'run ' },
      { text: 'akerdock logs', code: true },
      { text: ' now' },
    ]);
    expect(inlineRuns('**Restart** does not')).toEqual([
      { text: 'Restart', strong: true },
      { text: ' does not' },
    ]);
  });

  it('leaves an unclosed delimiter as literal text', () => {
    expect(inlineRuns('a lone ` backtick')).toEqual([{ text: 'a lone ` backtick' }]);
    expect(inlineRuns('2 ** 8')).toEqual([{ text: '2 ** 8' }]);
  });

  it('never drops characters, whatever the markup', () => {
    for (const text of ['`a`b**c**', 'plain', '**a** and `b`', '`` empty', '****']) {
      expect(inlineRuns(text).map((r) => r.text).join('').length).toBeLessThanOrEqual(text.length);
    }
    expect(inlineRuns('`a`b**c**').map((r) => r.text).join('')).toBe('abc');
  });
});

describe('narrowTopics', () => {
  const topics: DocTopic[] = [
    {
      id: 'open',
      title: 'Open',
      icon: 'circle',
      group: 'G',
      summary: 's',
      sections: [
        {
          id: 'a',
          title: 'A',
          blocks: [
            { kind: 'p', text: 'everyone' },
            { kind: 'p', text: 'writers only', permission: 'secrets:write' },
          ],
        },
        { id: 'b', title: 'B', permission: 'servers:manage', blocks: [{ kind: 'p', text: 'x' }] },
      ],
    },
    {
      id: 'gated',
      title: 'Gated',
      icon: 'circle',
      group: 'G',
      summary: 's',
      permission: 'instance:manage',
      sections: [{ id: 'a', title: 'A', blocks: [{ kind: 'p', text: 'x' }] }],
    },
    {
      id: 'root-only',
      title: 'Root',
      icon: 'circle',
      group: 'G',
      summary: 's',
      root: true,
      sections: [{ id: 'a', title: 'A', blocks: [{ kind: 'p', text: 'x' }] }],
    },
  ];

  it('drops a topic whose gate the session fails', () => {
    const visible = narrowTopics(topics, nobody, false);
    expect(visible.map((t) => t.id)).toEqual(['open']);
  });

  it('drops gated sections and blocks inside a visible topic', () => {
    const [open] = narrowTopics(topics, nobody, false);
    expect(open.sections.map((s) => s.id)).toEqual(['a']);
    expect(open.sections[0].blocks.length).toBe(1);
  });

  it('keeps a section once its permission is held', () => {
    const [open] = narrowTopics(topics, can(new Set(['servers:manage'])), false);
    expect(open.sections.map((s) => s.id)).toEqual(['a', 'b']);
  });

  it('gates root-only topics on the instance root flag, not on a permission', () => {
    expect(narrowTopics(topics, everybody, false).map((t) => t.id)).not.toContain('root-only');
    expect(narrowTopics(topics, everybody, true).map((t) => t.id)).toContain('root-only');
  });

  it('shows everything when asked to, gates included', () => {
    const visible = narrowTopics(topics, nobody, false, true);
    expect(visible.map((t) => t.id)).toEqual(['open', 'gated', 'root-only']);
    expect(visible[0].sections.length).toBe(2);
  });

  it('drops a topic left with no content rather than showing an empty page', () => {
    const emptied: DocTopic[] = [
      {
        id: 'hollow',
        title: 'Hollow',
        icon: 'circle',
        group: 'G',
        summary: 's',
        sections: [
          { id: 'a', title: 'A', blocks: [{ kind: 'p', text: 'x', permission: 'keys:reveal' }] },
        ],
      },
    ];
    expect(narrowTopics(emptied, nobody, false)).toEqual([]);
  });

  it('does not mutate the source content', () => {
    narrowTopics(topics, nobody, false);
    expect(topics[0].sections.length).toBe(2);
    expect(topics[0].sections[0].blocks.length).toBe(2);
  });
});

describe('searchTopics', () => {
  it('requires every term, and looks inside the blocks', () => {
    const found = searchTopics(DOC_TOPICS, 'fork preview');
    expect(found.map((t) => t.id)).toContain('previews');
    expect(searchTopics(DOC_TOPICS, 'port-forward').length).toBeGreaterThan(0);
    expect(searchTopics(DOC_TOPICS, 'zzz nothing')).toEqual([]);
  });

  it('returns everything on an empty query', () => {
    expect(searchTopics(DOC_TOPICS, '   ').length).toBe(DOC_TOPICS.length);
  });
});

describe('groupTopics', () => {
  it('keeps each group in order of first appearance', () => {
    const groups = groupTopics(DOC_TOPICS);
    expect(groups[0].title).toBe('Start here');
    expect(groups.map((g) => g.title)).toEqual([...new Set(DOC_TOPICS.map((t) => t.group))]);
    expect(groups.reduce((n, g) => n + g.topics.length, 0)).toBe(DOC_TOPICS.length);
  });
});

describe('the manual itself', () => {
  it('has unique topic ids, and unique section ids inside a topic', () => {
    const ids = DOC_TOPICS.map((t) => t.id);
    expect(new Set(ids).size).toBe(ids.length);
    for (const topic of DOC_TOPICS) {
      const sections = topic.sections.map((s) => s.id);
      expect(new Set(sections).size)
        .withContext(`duplicate section id in ${topic.id}`)
        .toBe(sections.length);
      expect(sections.length).toBeGreaterThan(0);
    }
  });

  it('only names icons that ship', () => {
    for (const topic of DOC_TOPICS) {
      expect(ICONS[topic.icon]).withContext(`unknown icon "${topic.icon}"`).toBeDefined();
    }
  });

  it('only gates on permissions the catalogue knows', () => {
    for (const topic of DOC_TOPICS) {
      const gates = [
        topic.permission,
        ...topic.sections.map((s) => s.permission),
        ...topic.sections.flatMap((s) => s.blocks.map((b) => b.permission)),
      ].filter((p): p is string => !!p);
      for (const gate of gates) {
        expect(CATALOGUE.has(gate)).withContext(`unknown permission "${gate}"`).toBeTrue();
      }
    }
  });

  it('only links to routes the dashboard declares', () => {
    for (const topic of DOC_TOPICS) {
      for (const link of topic.links ?? []) {
        if (!link.route) continue;
        const segment = link.route.replace(/^\//, '').split('/')[0];
        expect(ROUTE_SEGMENTS.has(segment)).withContext(`dead link ${link.route}`).toBeTrue();
      }
    }
  });

  it('gives every topic a summary and at least one block per section', () => {
    for (const topic of DOC_TOPICS) {
      expect(topic.summary.length).toBeGreaterThan(10);
      for (const section of topic.sections) {
        expect(section.blocks.length).withContext(`${topic.id}/${section.id}`).toBeGreaterThan(0);
      }
    }
  });

  it('keeps tables rectangular', () => {
    for (const topic of DOC_TOPICS) {
      for (const section of topic.sections) {
        for (const block of section.blocks) {
          if (block.kind !== 'table') continue;
          for (const row of block.rows) {
            expect(row.length).withContext(`${topic.id}/${section.id}`).toBe(block.head.length);
          }
        }
      }
    }
  });

  it('still reads as a manual for a plain member — the reader it is written for', () => {
    const visible = narrowTopics(DOC_TOPICS, can(MEMBER), false);
    const ids = visible.map((t) => t.id);
    // The daily surface must survive the filter…
    for (const expected of [
      'first-deploy',
      'applications',
      'env-vars',
      'deployments',
      'previews',
      'logs',
      'terminal',
      'port-forward',
      'databases',
      'cli',
      'account',
    ]) {
      expect(ids).withContext(`${expected} hidden from a member`).toContain(expected);
    }
    // …and instance administration must not.
    expect(ids).not.toContain('servers');
    expect(ids).not.toContain('instance');
  });

  it('shows a reviewer the previews page and does not pretend they can deploy', () => {
    const reviewer = new Set([
      'projects:read',
      'environments:read',
      'applications:read',
      'previews:read',
    ]);
    const visible = narrowTopics(DOC_TOPICS, can(reviewer), false);
    const ids = visible.map((t) => t.id);
    expect(ids).toContain('previews');
    expect(ids).not.toContain('env-vars');
    expect(ids).not.toContain('deployments');

    const previews = visible.find((t) => t.id === 'previews');
    expect(previews?.sections.map((s) => s.id)).not.toContain('forks');

    // A read-only role must not be taught which button to press to deploy.
    const applications = visible.find((t) => t.id === 'applications');
    expect(applications?.sections.map((s) => s.id)).not.toContain('lifecycle');
    expect(applications?.sections.map((s) => s.id)).not.toContain('settings');
  });
});
