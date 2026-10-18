import { expect, it } from 'vitest';
import { githubUrl, httpUrl, httpsUrl, markdownHref, taskPathId } from './url';

// One row per value: what each helper returns. null means the link is refused.
const REFUSED = null;

it.each<[unknown, string | null, string | null, string | null, string | null]>([
  // Scheme tricks: only the parsed scheme counts.
  ['javascript:alert(1)', REFUSED, REFUSED, REFUSED, REFUSED],
  ['JavaScript:alert(1)', REFUSED, REFUSED, REFUSED, REFUSED],
  ['data:text/html,<script>alert(1)</script>', REFUSED, REFUSED, REFUSED, REFUSED],
  ['mailto:someone@example.test', REFUSED, REFUSED, REFUSED, 'mailto:someone@example.test'],
  // Protocol-relative and relative values have no scheme to allow.
  ['//evil.example/o/r', REFUSED, REFUSED, REFUSED, REFUSED],
  ['/o/r/pull/1', REFUSED, REFUSED, REFUSED, REFUSED],
  // A same-origin task link is the one relative value `markdownHref` allows.
  [
    '/tasks/158f277e-83bc-4584-9b29-88d217542832',
    REFUSED,
    REFUSED,
    REFUSED,
    '/tasks/158f277e-83bc-4584-9b29-88d217542832'
  ],
  ['/tasks/158f277e-83bc-4584-9b29-88d217542832/', REFUSED, REFUSED, REFUSED, REFUSED],
  ['/tasks/', REFUSED, REFUSED, REFUSED, REFUSED],
  // Mixed-case scheme and host are the same URL, so they are accepted and normalised.
  [
    'HTTPS://GitHub.com/o/r',
    'https://github.com/o/r',
    'https://github.com/o/r',
    'https://github.com/o/r',
    'https://github.com/o/r'
  ],
  // Credentials are removed from the URL returned.
  [
    'https://user:pass@github.com/o/r',
    'https://github.com/o/r',
    'https://github.com/o/r',
    'https://github.com/o/r',
    'https://github.com/o/r'
  ],
  // github.com in the userinfo is not the host, so the GitHub rule refuses it.
  [
    'https://github.com@evil.example/o/r',
    'https://evil.example/o/r',
    'https://evil.example/o/r',
    REFUSED,
    'https://evil.example/o/r'
  ],
  // Lookalike GitHub hosts.
  [
    'https://github.com.evil.example/o/r',
    'https://github.com.evil.example/o/r',
    'https://github.com.evil.example/o/r',
    REFUSED,
    'https://github.com.evil.example/o/r'
  ],
  [
    'https://evilgithub.com/o/r',
    'https://evilgithub.com/o/r',
    'https://evilgithub.com/o/r',
    REFUSED,
    'https://evilgithub.com/o/r'
  ],
  // Whitespace is refused, not trimmed.
  [' https://github.com/o/r', REFUSED, REFUSED, REFUSED, REFUSED],
  ['https://github.com/o/r ', REFUSED, REFUSED, REFUSED, REFUSED],
  // Empty and non-string values.
  ['', REFUSED, REFUSED, REFUSED, REFUSED],
  [42, REFUSED, REFUSED, REFUSED, REFUSED],
  [null, REFUSED, REFUSED, REFUSED, REFUSED],
  // Plain http: allowed for previews and repository URLs, refused everywhere else.
  ['http://localhost:3000/', REFUSED, 'http://localhost:3000/', REFUSED, 'http://localhost:3000/'],
  ['git@example.test:repo.git', REFUSED, REFUSED, REFUSED, REFUSED],
  ['not a url', REFUSED, REFUSED, REFUSED, REFUSED]
])('%j', (value, https, http, github, markdown) => {
  expect(httpsUrl(value)).toBe(https);
  expect(httpUrl(value)).toBe(http);
  expect(githubUrl(value)).toBe(github);
  expect(markdownHref(value)).toBe(markdown);
});

it('accepts a github.com PR link as it is', () => {
  expect(githubUrl('https://github.com/owner/repo/pull/7')).toBe(
    'https://github.com/owner/repo/pull/7'
  );
});

it('reads the id out of a task link, and nothing else', () => {
  expect(taskPathId('/tasks/158f277e-83bc-4584-9b29-88d217542832')).toBe(
    '158f277e-83bc-4584-9b29-88d217542832'
  );
  expect(taskPathId('/tasks/158f277e-83bc-4584-9b29-88d217542832/extra')).toBeNull();
  expect(taskPathId('/sessions/1')).toBeNull();
  expect(taskPathId('https://evil.example/tasks/1')).toBeNull();
  expect(taskPathId(42)).toBeNull();
});
