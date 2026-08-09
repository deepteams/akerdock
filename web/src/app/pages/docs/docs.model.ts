/**
 * The in-app documentation's content model (§25).
 *
 * Two properties matter here and shape everything below.
 *
 * First, the content is DATA, not markup: a topic is a tree of typed blocks
 * the renderer walks. Nothing in `docs.content.ts` can inject HTML into the
 * page — the strongest inline markup a paragraph carries is `code` and
 * **strong**, parsed here into runs the template binds as text.
 *
 * Second, every level of that tree may name a permission. The reader of these
 * pages is a developer using the platform daily, not the operator who installed
 * it: a page that documents "add a server" to somebody who may not add one is
 * noise between them and the thing they came for. So the same `can()` that
 * filters the sidebar filters the manual, down to the individual section.
 */

/** A parsed piece of inline text — plain, `code` or **strong**. */
export interface DocRun {
  text: string;
  code?: boolean;
  strong?: boolean;
}

/** What a reader must hold to be shown a topic, a section or a block. */
export interface DocGate {
  /** Granular permission (rbac-matrix §1.2) the content is useless without. */
  permission?: string;
  /** Instance-root-only content (global settings, encryption, updates). */
  root?: boolean;
}

export type DocBlock = DocGate &
  (
    | { kind: 'p'; text: string }
    | { kind: 'ul'; items: string[] }
    | { kind: 'steps'; items: string[] }
    | { kind: 'code'; code: string; caption?: string }
    | { kind: 'note'; text: string }
    | { kind: 'warn'; text: string }
    | { kind: 'table'; head: string[]; rows: string[][] }
  );

/** A link out of the documentation: an app route, or an external URL. */
export interface DocLink {
  label: string;
  /** Router path inside the dashboard (`/applications`). */
  route?: string;
  /** Absolute URL, opened in a new tab. */
  href?: string;
}

export interface DocSection extends DocGate {
  id: string;
  title: string;
  blocks: DocBlock[];
}

export interface DocTopic extends DocGate {
  id: string;
  title: string;
  /** Icon name from the registry (ui/icon/icons.ts). */
  icon: string;
  /** One line, shown on the index card and in search results. */
  summary: string;
  /** Index group title — the order of first appearance is the display order. */
  group: string;
  /** Where in the dashboard the topic is exercised. */
  links?: DocLink[];
  sections: DocSection[];
}

/** Reads a gate against the session. `showAll` renders the manual whole. */
export function allowed(
  gate: DocGate,
  can: (permission: string) => boolean,
  isRoot: boolean,
  showAll = false,
): boolean {
  if (showAll) return true;
  if (gate.root && !isRoot) return false;
  if (gate.permission && !can(gate.permission)) return false;
  return true;
}

/**
 * A topic reduced to what this session may act on. Returns null when nothing
 * of it survives: a topic whose every section is gated away is a title with an
 * empty page under it, which reads as a bug rather than as an absence.
 */
export function narrowTopic(
  topic: DocTopic,
  can: (permission: string) => boolean,
  isRoot: boolean,
  showAll = false,
): DocTopic | null {
  if (!allowed(topic, can, isRoot, showAll)) return null;
  const sections = topic.sections
    .filter((section) => allowed(section, can, isRoot, showAll))
    .map((section) => ({
      ...section,
      blocks: section.blocks.filter((block) => allowed(block, can, isRoot, showAll)),
    }))
    .filter((section) => section.blocks.length > 0);
  if (sections.length === 0) return null;
  return { ...topic, sections };
}

export function narrowTopics(
  topics: readonly DocTopic[],
  can: (permission: string) => boolean,
  isRoot: boolean,
  showAll = false,
): DocTopic[] {
  return topics
    .map((topic) => narrowTopic(topic, can, isRoot, showAll))
    .filter((topic): topic is DocTopic => topic !== null);
}

/** The searchable text of a topic: everything a reader could remember of it. */
function haystack(topic: DocTopic): string {
  const parts: string[] = [topic.title, topic.summary, topic.group];
  for (const section of topic.sections) {
    parts.push(section.title);
    for (const block of section.blocks) {
      switch (block.kind) {
        case 'p':
        case 'note':
        case 'warn':
          parts.push(block.text);
          break;
        case 'ul':
        case 'steps':
          parts.push(...block.items);
          break;
        case 'code':
          parts.push(block.code, block.caption ?? '');
          break;
        case 'table':
          parts.push(...block.head, ...block.rows.flat());
          break;
      }
    }
  }
  return parts.join('\n').toLowerCase();
}

/**
 * Filters topics on a free-text query. Every whitespace-separated term must
 * appear somewhere in the topic — an AND, because "preview fork" should find
 * the one paragraph about approving a fork's preview, not both topics that
 * mention either word.
 */
export function searchTopics(topics: readonly DocTopic[], query: string): DocTopic[] {
  const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (terms.length === 0) return [...topics];
  return topics.filter((topic) => {
    const text = haystack(topic);
    return terms.every((term) => text.includes(term));
  });
}

/** Groups topics for the index, preserving each group's first appearance. */
export function groupTopics(topics: readonly DocTopic[]): { title: string; topics: DocTopic[] }[] {
  const groups: { title: string; topics: DocTopic[] }[] = [];
  for (const topic of topics) {
    const existing = groups.find((group) => group.title === topic.group);
    if (existing) existing.topics.push(topic);
    else groups.push({ title: topic.group, topics: [topic] });
  }
  return groups;
}

/**
 * Splits inline text into runs. `code` spans and **strong** spans are the
 * whole vocabulary: anything richer would be markup, and markup in a data file
 * ends up rendered with innerHTML sooner or later.
 *
 * An unclosed delimiter is text, not an error — the manual must render even
 * when a sentence legitimately contains a lone backtick or asterisk.
 */
export function inlineRuns(text: string): DocRun[] {
  const runs: DocRun[] = [];
  let plain = '';
  const flush = () => {
    if (plain) runs.push({ text: plain });
    plain = '';
  };
  for (let i = 0; i < text.length; ) {
    const rest = text.slice(i);
    if (rest.startsWith('`')) {
      const end = text.indexOf('`', i + 1);
      if (end > i + 1) {
        flush();
        runs.push({ text: text.slice(i + 1, end), code: true });
        i = end + 1;
        continue;
      }
    }
    if (rest.startsWith('**')) {
      const end = text.indexOf('**', i + 2);
      if (end > i + 1) {
        flush();
        runs.push({ text: text.slice(i + 2, end), strong: true });
        i = end + 2;
        continue;
      }
    }
    plain += text[i];
    i += 1;
  }
  flush();
  return runs;
}
