import { describe, expect, it } from 'vitest';
import tokens from '../../../design/tokens.json';
import { cn } from './cn';

describe('cn', () => {
  it('keeps a token font size beside a text colour', () => {
    expect(cn('text-ui', 'text-foreground')).toBe('text-ui text-foreground');
    expect(cn('text-2xs', 'text-muted-foreground')).toBe('text-2xs text-muted-foreground');
    expect(cn('text-ui hover:text-accent-foreground', 'text-accent-foreground')).toContain(
      'text-ui'
    );
  });

  it('lets a later size replace an earlier one, token or not', () => {
    expect(cn('text-sm', 'text-ui')).toBe('text-ui');
    expect(cn('text-ui', 'text-xs')).toBe('text-xs');
    expect(cn('text-2xs', 'text-ui')).toBe('text-ui');
  });

  it('knows every font size the tokens define', () => {
    // A size added to the tokens must be added to cn as well, or a colour class will drop it.
    const builtIn = ['xs', 'sm', 'base', 'lg', 'xl', '2xl', '3xl'];
    const sizes = Object.keys(tokens.font.size).filter((key) => !key.startsWith('$'));
    for (const size of sizes.filter((key) => !builtIn.includes(key)))
      expect(cn(`text-${size}`, 'text-foreground'), size).toBe(`text-${size} text-foreground`);
  });
});
