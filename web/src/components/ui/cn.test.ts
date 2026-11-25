import { describe, expect, it } from 'vitest';
import tokens from '../../../design/tokens.json';
import { cn } from './cn';

describe('cn', () => {
  it('keeps a token font size beside a text colour', () => {
    expect(cn('text-body', 'text-foreground')).toBe('text-body text-foreground');
    expect(cn('text-label', 'text-muted-foreground')).toBe('text-label text-muted-foreground');
    expect(cn('text-body hover:text-accent-foreground', 'text-accent-foreground')).toContain(
      'text-body'
    );
  });

  it('lets a later size replace an earlier one', () => {
    expect(cn('text-body', 'text-caption')).toBe('text-caption');
    expect(cn('text-label', 'text-body')).toBe('text-body');
    expect(cn('text-title', 'text-display')).toBe('text-display');
  });

  it('knows every font size the tokens define', () => {
    // A size added to the tokens must be added to cn as well, or a colour class will drop it.
    const sizes = Object.keys(tokens.font.size).filter((key) => !key.startsWith('$'));
    expect(sizes.length).toBeGreaterThan(0);
    for (const size of sizes)
      expect(cn(`text-${size}`, 'text-foreground'), size).toBe(`text-${size} text-foreground`);
  });
});
