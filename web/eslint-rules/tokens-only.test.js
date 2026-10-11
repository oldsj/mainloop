import { describe, expect, it } from 'vitest';
import { problemsIn } from './tokens-only.js';

describe('tokens-only', () => {
  it.each(['rounded-none', 'md:hover:rounded-none', 'rounded-t-none', 'bg-card text-sm p-md'])(
    'accepts %s',
    (text) => expect(problemsIn(text)).toEqual([])
  );

  it.each([
    'rounded',
    'rounded-full',
    'rounded-2xl',
    'rounded-t-lg',
    'rounded-[4px]',
    'hover:rounded-md',
    'bg-red-500',
    'text-white',
    'border-neutral-200',
    '#fff',
    'rgb(0 0 0)',
    'oklch(0.5 0 0)'
  ])('rejects %s', (text) => expect(problemsIn(text)).not.toEqual([]));
});
