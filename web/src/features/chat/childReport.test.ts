import { describe, expect, it } from 'vitest';
import { parseChildReport } from './childReport';

describe('parseChildReport', () => {
  it('reads the child id, title and body', () => {
    expect(
      parseChildReport("[report from child 0a1b2c3d 'Fix the build']\nDone. Tests pass.")
    ).toEqual({
      childId: '0a1b2c3d',
      title: 'Fix the build',
      fallback: false,
      body: 'Done. Tests pass.'
    });
  });

  it('marks a report the child never wrote', () => {
    const report = parseChildReport("[report from child 0a1b2c3d 'Fix' fallback]\nended");
    expect(report?.fallback).toBe(true);
    expect(report?.body).toBe('ended');
  });

  it.each([
    ['plain text', 'hello'],
    ['a header that is not first', "text [report from child 0a1b2c3d 'x']\nbody"],
    ['a short child id', "[report from child 0a1b 'x']\nbody"],
    ['an uppercase child id', "[report from child 0A1B2C3D 'x']\nbody"]
  ])('returns null for %s', (_name, content) => {
    expect(parseChildReport(content)).toBeNull();
  });
});
