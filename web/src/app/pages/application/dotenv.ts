/**
 * Rendering and parsing of the .env dev views of the environment variables
 * tab: one KEY=value line per variable, dotenv-style double quotes for values
 * a line-based parse would mangle.
 */

/** The value shown in place of a secret the API will never return: keeping the
 * line as-is means "leave it alone", so it must survive a render/parse
 * round-trip unchanged. */
export const REDACTED = '(redacted)';

/** Renders one value for the .env text: dotenv-style double quotes when the
 * raw form would not survive a line-based parse (newlines, edge whitespace). */
export function quoteEnvValue(value: string): string {
  if (!value.includes('\n') && !/^\s|\s$/.test(value) && !value.startsWith('"')) {
    return value;
  }
  return `"${value.replace(/\\/g, '\\\\').replace(/"/g, '\\"').replace(/\n/g, '\\n')}"`;
}

/** Parses .env text: KEY=value lines, blank lines and #-comments ignored,
 * double-quoted values unescaped, the last occurrence of a duplicate key
 * wins. Line numbers of unparsable lines come back as errors. */
export function parseDotenv(text: string): { entries: Map<string, string>; errors: string[] } {
  const entries = new Map<string, string>();
  const errors: string[] = [];
  text.split('\n').forEach((line, index) => {
    const trimmed = line.trim();
    if (trimmed === '' || trimmed.startsWith('#')) return;
    const eq = trimmed.indexOf('=');
    if (eq <= 0) {
      errors.push(`line ${index + 1} has no KEY=value form`);
      return;
    }
    const key = trimmed.slice(0, eq).trim();
    let value = trimmed.slice(eq + 1);
    if (value.length >= 2 && value.startsWith('"') && value.endsWith('"')) {
      value = value
        .slice(1, -1)
        .replace(/\\n/g, '\n')
        .replace(/\\"/g, '"')
        .replace(/\\\\/g, '\\');
    }
    entries.set(key, value);
  });
  return { entries, errors };
}
