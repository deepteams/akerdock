import {
  FALLBACK_TIME_ZONES,
  browserTimeZone,
  filterTimeZones,
  formatOccurrence,
  formatOffset,
  isKnownTimeZone,
  offsetMinutes,
  supportedTimeZones,
  timeZoneLabel,
  timeZoneOptions,
} from './timezone';

// Summer and winter of the same year: the whole point of showing an offset is
// that Europe/Paris is not one offset but two.
const SUMMER = new Date('2026-07-01T12:00:00Z');
const WINTER = new Date('2026-01-15T12:00:00Z');

/** Runs `body` with `Intl.supportedValuesOf` replaced (or removed). */
function withSupportedValuesOf(replacement: PropertyDescriptor | null, body: () => void): void {
  const holder = Intl as unknown as Record<string, unknown>;
  const original = Object.getOwnPropertyDescriptor(holder, 'supportedValuesOf');
  try {
    delete holder['supportedValuesOf'];
    if (replacement) Object.defineProperty(holder, 'supportedValuesOf', replacement);
    body();
  } finally {
    delete holder['supportedValuesOf'];
    if (original) Object.defineProperty(holder, 'supportedValuesOf', original);
  }
}

describe('timezone list', () => {
  it('offers UTC first and exactly once', () => {
    const zones = supportedTimeZones();

    expect(zones[0]).toBe('UTC');
    expect(zones.filter((zone) => zone === 'UTC').length).toBe(1);
    expect(zones.length).toBeGreaterThan(FALLBACK_TIME_ZONES.length);
    expect(zones).toContain('Europe/Paris');
  });

  it('sorts everything after UTC alphabetically', () => {
    const rest = supportedTimeZones().slice(1);
    const sorted = [...rest].sort((a, b) => a.localeCompare(b));

    expect(rest).toEqual(sorted);
  });

  // Some engines have no Intl.supportedValuesOf at all: a short list beats an
  // empty form.
  it('falls back to the hardcoded list when the engine has no supportedValuesOf', () => {
    withSupportedValuesOf(null, () => {
      const zones = supportedTimeZones();

      expect(zones[0]).toBe('UTC');
      expect(zones.length).toBe(FALLBACK_TIME_ZONES.length);
      expect(zones).toContain('Europe/Paris');
      expect(zones).toContain('America/New_York');
    });
  });

  it('falls back when the engine throws on supportedValuesOf', () => {
    withSupportedValuesOf(
      {
        configurable: true,
        get: () => {
          throw new Error('unsupported');
        },
      },
      () => expect(supportedTimeZones().length).toBe(FALLBACK_TIME_ZONES.length),
    );
  });

  it('falls back when the engine returns an empty list', () => {
    withSupportedValuesOf({ configurable: true, value: () => [] }, () =>
      expect(supportedTimeZones().length).toBe(FALLBACK_TIME_ZONES.length),
    );
  });

  it('falls back when the engine answers nothing at all', () => {
    withSupportedValuesOf({ configurable: true, value: () => undefined }, () =>
      expect(supportedTimeZones().length).toBe(FALLBACK_TIME_ZONES.length),
    );
  });

  it('reports the browser zone', () => {
    expect(browserTimeZone().length).toBeGreaterThan(0);
  });

  it('falls back to UTC when the engine will not name the browser zone', () => {
    const holder = Intl as unknown as Record<string, unknown>;
    const original = holder['DateTimeFormat'];
    try {
      holder['DateTimeFormat'] = () => ({ resolvedOptions: () => ({ timeZone: '' }) });
      expect(browserTimeZone()).toBe('UTC');
      holder['DateTimeFormat'] = () => {
        throw new Error('no Intl here');
      };
      expect(browserTimeZone()).toBe('UTC');
    } finally {
      holder['DateTimeFormat'] = original;
    }
  });

  it('knows which names the engine database accepts', () => {
    expect(isKnownTimeZone('Europe/Paris')).toBeTrue();
    expect(isKnownTimeZone('Mars/Phobos')).toBeFalse();
  });
});

describe('utc offsets', () => {
  it('formats the current offset of a zone', () => {
    expect(formatOffset('UTC', SUMMER)).toBe('UTC+00:00');
    expect(formatOffset('Europe/Paris', SUMMER)).toBe('UTC+02:00');
    expect(formatOffset('Asia/Tokyo', SUMMER)).toBe('UTC+09:00');
  });

  it('follows daylight saving instead of a fixed table', () => {
    expect(formatOffset('Europe/Paris', WINTER)).toBe('UTC+01:00');
    expect(formatOffset('America/New_York', SUMMER)).toBe('UTC-04:00');
    expect(formatOffset('America/New_York', WINTER)).toBe('UTC-05:00');
  });

  it('keeps half-hour and quarter-hour offsets', () => {
    expect(formatOffset('Asia/Kolkata', SUMMER)).toBe('UTC+05:30');
    expect(formatOffset('Asia/Kathmandu', SUMMER)).toBe('UTC+05:45');
    expect(offsetMinutes('Asia/Kolkata', SUMMER)).toBe(330);
  });

  it('says nothing rather than lying about an unknown zone', () => {
    expect(offsetMinutes('Mars/Phobos', SUMMER)).toBeNull();
    expect(formatOffset('Mars/Phobos', SUMMER)).toBe('');
    expect(timeZoneLabel('Mars/Phobos', SUMMER)).toBe('Mars/Phobos');
  });

  it('labels a zone with its offset', () => {
    expect(timeZoneLabel('Europe/Paris', SUMMER)).toBe('Europe/Paris (UTC+02:00)');
  });

  it('reads the current instant when none is given', () => {
    expect(offsetMinutes('UTC')).toBe(0);
    expect(formatOffset('UTC')).toBe('UTC+00:00');
    expect(timeZoneLabel('UTC')).toBe('UTC (UTC+00:00)');
    expect(timeZoneOptions()[0]).toEqual(
      jasmine.objectContaining({ zone: 'UTC', offset: 'UTC+00:00' }),
    );
  });

  it('is not fooled by an instant carrying milliseconds', () => {
    expect(offsetMinutes('Europe/Paris', new Date('2026-07-01T12:00:00.750Z'))).toBe(120);
  });
});

describe('option list', () => {
  const zones = ['UTC', 'Europe/Paris', 'America/New_York', 'Asia/Kolkata'];

  it('carries the IANA name as value and the offset in the label', () => {
    const options = timeZoneOptions(null, SUMMER, zones);

    expect(options.map((option) => option.zone)).toEqual(zones);
    expect(options[1].label).toBe('Europe/Paris (UTC+02:00)');
    expect(options[1].offset).toBe('UTC+02:00');
  });

  // Opening an edit form must never rewrite what was stored, even if this
  // engine's database has never heard of the zone.
  it('keeps a stored zone the engine does not know', () => {
    const options = timeZoneOptions('Mars/Phobos', SUMMER, zones);

    expect(options.map((option) => option.zone)).toContain('Mars/Phobos');
    expect(options[options.length - 1].label).toBe('Mars/Phobos');
  });

  it('does not duplicate a stored zone already in the list', () => {
    const options = timeZoneOptions('Europe/Paris', SUMMER, zones);

    expect(options.filter((option) => option.zone === 'Europe/Paris').length).toBe(1);
  });

  it('filters on the name, underscores read as spaces', () => {
    const options = timeZoneOptions(null, SUMMER, zones);

    expect(filterTimeZones(options, 'new york').map((o) => o.zone)).toEqual(['America/New_York']);
    expect(filterTimeZones(options, 'PARIS').map((o) => o.zone)).toEqual(['Europe/Paris']);
    expect(filterTimeZones(options, 'nowhere')).toEqual([]);
  });

  it('filters on the offset too', () => {
    const options = timeZoneOptions(null, SUMMER, zones);

    expect(filterTimeZones(options, '+05:30').map((o) => o.zone)).toEqual(['Asia/Kolkata']);
  });

  it('returns everything for a blank query', () => {
    const options = timeZoneOptions(null, SUMMER, zones);

    expect(filterTimeZones(options, '   ').length).toBe(zones.length);
  });
});

describe('occurrence rendering', () => {
  const INSTANT = '2026-08-10T01:00:00Z';

  it('renders the instant in the resource zone, named', () => {
    const label = formatOccurrence(INSTANT, 'Europe/Paris', 'Europe/Paris');

    expect(label).toEqual({ primary: '10 Aug 2026, 03:00 Europe/Paris', secondary: null });
  });

  it('adds the viewer local time when the zones differ', () => {
    const label = formatOccurrence(INSTANT, 'UTC', 'Asia/Tokyo');

    expect(label?.primary).toBe('10 Aug 2026, 01:00 UTC');
    expect(label?.secondary).toBe('10 Aug 2026, 10:00 your time');
  });

  it('treats a missing zone as UTC', () => {
    expect(formatOccurrence(INSTANT, null, 'UTC')?.primary).toBe('10 Aug 2026, 01:00 UTC');
    expect(formatOccurrence(INSTANT, '', 'UTC')?.primary).toBe('10 Aug 2026, 01:00 UTC');
  });

  it('still shows an hour when the stored zone is unknown to the engine', () => {
    const label = formatOccurrence(INSTANT, 'Mars/Phobos', 'UTC');

    expect(label).toEqual({ primary: '10 Aug 2026, 01:00 UTC', secondary: null });
  });

  it('renders nothing when there is no occurrence or the instant is unusable', () => {
    expect(formatOccurrence(null, 'UTC', 'UTC')).toBeNull();
    expect(formatOccurrence(undefined, 'UTC', 'UTC')).toBeNull();
    expect(formatOccurrence('not-a-date', 'UTC', 'UTC')).toBeNull();
  });

  it('drops the local line when the viewer cannot be rendered either', () => {
    expect(formatOccurrence(INSTANT, 'UTC', 'Mars/Phobos')).toEqual({
      primary: '10 Aug 2026, 01:00 UTC',
      secondary: null,
    });
  });

  it('defaults the viewer zone to the browser', () => {
    expect(formatOccurrence(INSTANT, 'UTC')?.primary).toBe('10 Aug 2026, 01:00 UTC');
  });
});
