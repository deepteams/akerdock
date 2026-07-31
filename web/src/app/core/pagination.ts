/** One page of a cursor-paginated list call. */
interface Page<T> {
  data: T[];
  next_cursor?: string | null;
}

/**
 * Follows next_cursor until the collection is exhausted, so lists render every
 * row instead of silently truncating at the first page. The page cap is a
 * runaway guard (50 × limit 100 = 5 000 rows), not a feature: a list that
 * genuinely outgrows it deserves real pagination UI, not a bigger cap.
 */
export async function fetchAll<T>(
  fetchPage: (cursor?: string) => Promise<Page<T>>,
  maxPages = 50,
): Promise<T[]> {
  const all: T[] = [];
  let cursor: string | undefined;
  for (let page = 0; page < maxPages; page++) {
    const res = await fetchPage(cursor);
    all.push(...res.data);
    if (!res.next_cursor) break;
    cursor = res.next_cursor;
  }
  return all;
}
