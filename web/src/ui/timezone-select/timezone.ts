// Everything the dashboard knows about IANA timezones. The rule the UI has to
// serve is simple: a cron is written in a zone, and the operator must be able
// to tell, without arithmetic, at what wall-clock hour it will actually fire.
// So a zone is never shown as a bare name — it always carries its *current*
// UTC offset, and an occurrence is always shown in the resource's own zone,
// with the viewer's local time beside it when the two differ.
//
// No zone database is bundled: the engine already has one behind `Intl`.

/**
 * Zones offered when the engine has no `Intl.supportedValuesOf` (it is recent,
 * and a few engines still lack it). Deliberately short — one entry per common
 * operational region. A form that renders no option at all is worse than a
 * form that renders twenty; typing a zone that is missing here is not possible,
 * but a stored zone outside the list is always kept and always displayed
 * (see `timeZoneOptions`).
 */
export const FALLBACK_TIME_ZONES: readonly string[] = [
  'UTC',
  'Africa/Cairo',
  'Africa/Johannesburg',
  'Africa/Lagos',
  'America/Chicago',
  'America/Denver',
  'America/Los_Angeles',
  'America/Mexico_City',
  'America/New_York',
  'America/Sao_Paulo',
  'America/Toronto',
  'Asia/Dubai',
  'Asia/Hong_Kong',
  'Asia/Jakarta',
  'Asia/Kolkata',
  'Asia/Seoul',
  'Asia/Shanghai',
  'Asia/Singapore',
  'Asia/Tokyo',
  'Australia/Melbourne',
  'Australia/Sydney',
  'Europe/Amsterdam',
  'Europe/Berlin',
  'Europe/Dublin',
  'Europe/Istanbul',
  'Europe/Lisbon',
  'Europe/London',
  'Europe/Madrid',
  'Europe/Moscow',
  'Europe/Paris',
  'Europe/Rome',
  'Europe/Stockholm',
  'Europe/Warsaw',
  'Europe/Zurich',
  'Pacific/Auckland',
];

/** One entry of the selector: the value sent to the API plus what is shown. */
export interface TimeZoneOption {
  /** IANA name — this, and only this, is the value the API receives. */
  zone: string;
  /** Current offset, e.g. `UTC+02:00`; empty when the engine rejects the zone. */
  offset: string;
  /** `Europe/Paris (UTC+02:00)`, or just the name when the offset is unknown. */
  label: string;
  /** Lower-cased haystack for the filter box (underscores read as spaces). */
  search: string;
}

/**
 * Every zone the engine knows, `UTC` first and exactly once. The rest keeps a
 * plain alphabetical order: the list is browsed through the filter box, not by
 * scrolling, so grouping by offset would only make a zone harder to find.
 */
export function supportedTimeZones(): string[] {
  let zones: string[] = [];
  try {
    // `supportedValuesOf` is not in every engine's lib either — hence the cast.
    const supported = (
      Intl as unknown as { supportedValuesOf?: (key: string) => string[] }
    ).supportedValuesOf;
    if (typeof supported === 'function') zones = supported.call(Intl, 'timeZone') ?? [];
  } catch {
    zones = [];
  }
  if (zones.length === 0) zones = [...FALLBACK_TIME_ZONES];
  const rest = [...new Set(zones)]
    .filter((zone) => zone !== 'UTC')
    .sort((a, b) => a.localeCompare(b));
  return ['UTC', ...rest];
}

/** The browser's own zone, `UTC` when the engine will not say (§24.3). */
export function browserTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  } catch {
    return 'UTC';
  }
}

/** Whether the engine's zone database accepts this name. */
export function isKnownTimeZone(zone: string): boolean {
  try {
    new Intl.DateTimeFormat('en-GB', { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

// Formatters are not cheap to build and the tables rebuild labels on every
// change detection pass; one per (zone, kind) is plenty.
const partsFormatters = new Map<string, Intl.DateTimeFormat>();
const stampFormatters = new Map<string, Intl.DateTimeFormat>();

function partsFormatter(zone: string): Intl.DateTimeFormat {
  let formatter = partsFormatters.get(zone);
  if (!formatter) {
    formatter = new Intl.DateTimeFormat('en-GB', {
      timeZone: zone,
      hour12: false,
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    });
    partsFormatters.set(zone, formatter);
  }
  return formatter;
}

function stampFormatter(zone: string): Intl.DateTimeFormat {
  let formatter = stampFormatters.get(zone);
  if (!formatter) {
    formatter = new Intl.DateTimeFormat('en-GB', {
      timeZone: zone,
      hour12: false,
      year: 'numeric',
      month: 'short',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
    });
    stampFormatters.set(zone, formatter);
  }
  return formatter;
}

/**
 * Offset of `zone` at `at`, in minutes east of UTC. Read from the engine's own
 * rendering of the instant rather than from a table: that is what makes it
 * DST-correct — "0 3 * * *" in Europe/Paris is +02:00 in July and +01:00 in
 * January, and the operator is choosing the zone in one of the two.
 *
 * `null` when the engine does not know the zone.
 */
export function offsetMinutes(zone: string, at: Date = new Date()): number | null {
  let parts: Intl.DateTimeFormatPart[];
  try {
    parts = partsFormatter(zone).formatToParts(at);
  } catch {
    return null;
  }
  const found: Record<string, string> = {};
  for (const part of parts) found[part.type] = part.value;
  const year = Number(found['year']);
  const month = Number(found['month']);
  const day = Number(found['day']);
  // Some engines render midnight as hour 24 under hour12:false.
  const hour = Number(found['hour']) % 24;
  const minute = Number(found['minute']);
  const second = Number(found['second']);
  if ([year, month, day, hour, minute, second].some((value) => Number.isNaN(value))) return null;
  const asIfUtc = Date.UTC(year, month - 1, day, hour, minute, second);
  // Drop the milliseconds the formatted side cannot carry, or a non-zero ms
  // instant would shift the result by a fraction of a minute.
  const instant = Math.floor(at.getTime() / 1000) * 1000;
  return Math.round((asIfUtc - instant) / 60000);
}

/** `UTC+02:00`, `UTC-05:30`, `UTC+00:00` — empty for a zone the engine rejects. */
export function formatOffset(zone: string, at: Date = new Date()): string {
  const minutes = offsetMinutes(zone, at);
  if (minutes === null) return '';
  const sign = minutes < 0 ? '-' : '+';
  const absolute = Math.abs(minutes);
  const hours = String(Math.floor(absolute / 60)).padStart(2, '0');
  const rest = String(absolute % 60).padStart(2, '0');
  return `UTC${sign}${hours}:${rest}`;
}

/** `Europe/Paris (UTC+02:00)` — the name alone when the offset is unknown. */
export function timeZoneLabel(zone: string, at: Date = new Date()): string {
  const offset = formatOffset(zone, at);
  return offset ? `${zone} (${offset})` : zone;
}

/**
 * The option list for the selector. `extra` is the value currently stored on
 * the resource: it is added even when the engine's database does not contain
 * it (an alias, or a zone from a newer database), because opening an edit form
 * must never silently rewrite what was saved.
 */
export function timeZoneOptions(
  extra?: string | null,
  at: Date = new Date(),
  zones: string[] = supportedTimeZones(),
): TimeZoneOption[] {
  const all = [...zones];
  if (extra && !all.includes(extra)) all.push(extra);
  return all.map((zone) => {
    const offset = formatOffset(zone, at);
    return {
      zone,
      offset,
      label: offset ? `${zone} (${offset})` : zone,
      search: `${zone} ${offset}`.toLowerCase().replace(/_/g, ' '),
    };
  });
}

/** Substring match over the name (underscores read as spaces) and the offset. */
export function filterTimeZones(options: TimeZoneOption[], query: string): TimeZoneOption[] {
  const needle = query.trim().toLowerCase().replace(/_/g, ' ');
  if (!needle) return options;
  return options.filter((option) => option.search.includes(needle));
}

/**
 * How an occurrence is rendered in a table. `primary` is the instant in the
 * *resource's* zone — the one the cron was written in — and `secondary` is the
 * same instant in the viewer's zone, present only when the two disagree. That
 * second line is the whole point: a Paris operator reading a UTC plan should
 * not have to do the arithmetic that made them open the page.
 */
export interface OccurrenceLabel {
  primary: string;
  secondary: string | null;
}

function stamp(iso: string, zone: string): string | null {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return null;
  try {
    return stampFormatter(zone).format(date);
  } catch {
    return null;
  }
}

/**
 * `null` when there is nothing to show (no occurrence, or an instant the
 * browser cannot parse) — the caller renders its own em dash.
 */
export function formatOccurrence(
  iso: string | null | undefined,
  zone: string | null | undefined,
  viewerZone: string = browserTimeZone(),
): OccurrenceLabel | null {
  if (!iso) return null;
  // A stored zone the engine cannot render still has to show an hour: fall
  // back to UTC for the rendering, and name the zone actually used.
  let shown = zone || 'UTC';
  let primary = stamp(iso, shown);
  if (primary === null && shown !== 'UTC') {
    shown = 'UTC';
    primary = stamp(iso, shown);
  }
  if (primary === null) return null;
  const label = `${primary} ${shown}`;
  if (viewerZone === shown) return { primary: label, secondary: null };
  const local = stamp(iso, viewerZone);
  return { primary: label, secondary: local ? `${local} your time` : null };
}
