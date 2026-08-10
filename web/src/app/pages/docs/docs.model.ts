/**
 * The in-app manual's client-side model (§25.4, ADR-072).
 *
 * The manual used to be a TypeScript array of typed block literals compiled
 * into the bundle, and this file used to own both its vocabulary and its
 * permission filter. Neither lives here any more:
 *
 * - the corpus is Markdown under `internal/docs/manual/`, parsed at boot and
 *   served as HTML by `GET /docs` — the types below are the API's, not ours;
 * - the permission filter runs SERVER-SIDE. A topic or a section the reader
 *   may not open is simply not in the response, so there is nothing left to
 *   narrow here. Re-filtering the answer would be dead code wearing the
 *   costume of a control.
 *
 * What does stay client-side is search: the API ships, beside every chunk of
 * `html`, the same prose as plain `text`, precisely so a query can be matched
 * on words rather than on markup without a round trip per keystroke.
 */

import type { components } from '../../../api/schema';

export type DocLink = components['schemas']['ManualLink'];
export type DocSection = components['schemas']['ManualSection'];
export type DocTopic = components['schemas']['ManualTopic'];

/**
 * True when the server returned this topic or section only to be marked —
 * `?all=true` keeps what the reader's role does not reach, flagged, instead of
 * dropping it. The flag is the server's verdict; the UI never recomputes it.
 */
export function isBeyondRole(item: { beyond_role?: boolean }): boolean {
  return item.beyond_role === true;
}

/** The searchable text of a topic: everything a reader could remember of it. */
function haystack(topic: DocTopic): string {
  const parts: string[] = [topic.title, topic.summary, topic.group, topic.intro_text ?? ''];
  for (const section of topic.sections) {
    parts.push(section.title, section.text);
  }
  return parts.join('\n').toLowerCase();
}

/**
 * Filters topics on a free-text query. Every whitespace-separated term must
 * appear somewhere in the topic — an AND, because "preview fork" should find
 * the one page about approving a fork's preview, not both pages that mention
 * either word.
 */
export function searchTopics(topics: readonly DocTopic[], query: string): DocTopic[] {
  const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (terms.length === 0) return [...topics];
  return topics.filter((topic) => {
    const text = haystack(topic);
    return terms.every((term) => text.includes(term));
  });
}

/**
 * Groups topics for the index, preserving each group's first appearance.
 *
 * The API returns the manual in reading order — group by group, and in the
 * author's order inside each group. That order is meaning, so nothing here
 * sorts: the groups come out in the order they first appear, and the topics in
 * the order they arrived.
 */
export function groupTopics(topics: readonly DocTopic[]): { title: string; topics: DocTopic[] }[] {
  const groups: { title: string; topics: DocTopic[] }[] = [];
  for (const topic of topics) {
    const existing = groups.find((group) => group.title === topic.group);
    if (existing) existing.topics.push(topic);
    else groups.push({ title: topic.group, topics: [topic] });
  }
  return groups;
}

/** Anchors are namespaced: a section id is unique inside its topic only. */
export function sectionAnchor(topicId: string, sectionId: string): string {
  return `${topicId}--${sectionId}`;
}
