import { parseDotenv, quoteEnvValue } from './dotenv';

describe('parseDotenv', () => {
  it('parses KEY=value lines, skipping blanks and comments', () => {
    const { entries, errors } = parseDotenv('FOO=bar\n\n# comment\nBASE_URL=https://x?a=b=c\n');
    expect(errors).toEqual([]);
    expect(entries.get('FOO')).toBe('bar');
    // Everything after the FIRST = is the value, further = included.
    expect(entries.get('BASE_URL')).toBe('https://x?a=b=c');
  });

  it('keeps the last occurrence of a duplicate key', () => {
    const { entries } = parseDotenv('A=1\nA=2');
    expect(entries.get('A')).toBe('2');
  });

  it('reports unparsable lines with their line number', () => {
    const { errors } = parseDotenv('FOO=1\nnot a var\n=nokey');
    expect(errors.length).toBe(2);
    expect(errors[0]).toContain('line 2');
    expect(errors[1]).toContain('line 3');
  });

  it('unquotes double-quoted values with escapes', () => {
    const { entries } = parseDotenv('PEM="line1\\nline2"\nQ="say \\"hi\\""');
    expect(entries.get('PEM')).toBe('line1\nline2');
    expect(entries.get('Q')).toBe('say "hi"');
  });
});

describe('quoteEnvValue', () => {
  it('leaves plain values untouched', () => {
    expect(quoteEnvValue('bar')).toBe('bar');
    expect(quoteEnvValue('(redacted)')).toBe('(redacted)');
  });

  it('round-trips values that need quoting through parseDotenv', () => {
    for (const value of ['line1\nline2', ' padded ', 'say "hi"', '"quoted"']) {
      const { entries, errors } = parseDotenv(`K=${quoteEnvValue(value)}`);
      expect(errors).toEqual([]);
      expect(entries.get('K')).toBe(value);
    }
  });
});
