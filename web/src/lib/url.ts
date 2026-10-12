/**
 * URL-safety rules for links that come from the server, an agent or a provider. The scheme and
 * host are read from the URL parser's result, never from the raw string.
 *
 * - `httpsUrl`: https only (PR, CI and project links);
 * - `httpUrl`: http and https (dev previews and declared repository URLs);
 * - `githubUrl`: https on github.com only;
 * - `markdownHref`: http, https and mailto (links in agent text).
 *
 * A value with surrounding whitespace is refused, not trimmed. Credentials are removed from the URL
 * that is returned. A refused value returns null, so the caller shows the words without a link.
 */

const HTTPS = ['https:'];
const HTTP_OR_HTTPS = ['http:', 'https:'];
const MARKDOWN = ['http:', 'https:', 'mailto:'];

function allowedUrl(value: unknown, protocols: string[], host?: string): string | null {
  if (typeof value !== 'string' || value.trim() !== value) return null;
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return null;
  }
  if (!protocols.includes(url.protocol)) return null;
  if (host !== undefined && url.hostname !== host) return null;
  url.username = '';
  url.password = '';
  return url.toString();
}

export function httpsUrl(value: unknown): string | null {
  return allowedUrl(value, HTTPS);
}

export function httpUrl(value: unknown): string | null {
  return allowedUrl(value, HTTP_OR_HTTPS);
}

export function githubUrl(value: unknown): string | null {
  return allowedUrl(value, HTTPS, 'github.com');
}

/** For react-markdown's `urlTransform`: a refused link loses its target and keeps its words. */
export function markdownHref(value: unknown): string | null {
  return allowedUrl(value, MARKDOWN);
}
