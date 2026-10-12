import { describe, expect, it } from 'vitest';
import { problemsIn } from './tokens-only.js';

describe('tokens-only', () => {
  it.each([
    'rounded-none',
    'md:hover:rounded-none',
    'rounded-t-none',
    'bg-card text-sm p-md',
    'data-[state=open]:rounded-none',
    'has-[>svg]:bg-card',
    'ring-offset-background inset-ring-border'
  ])('accepts %s', (text) => expect(problemsIn(text)).toEqual([]));

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
    'oklch(0.5 0 0)',
    // Variants written with brackets.
    'data-[state=open]:bg-red-500',
    'data-[state=open]:rounded-lg',
    'aria-[expanded=true]:rounded-md',
    'has-[>svg]:rounded-sm',
    'group-data-[x]:text-white',
    'max-[600px]:rounded-lg',
    '[&:hover]:bg-blue-500',
    // The important marker, either side.
    '!bg-red-500',
    'bg-red-500!',
    // Palettes new in Tailwind 4.3.
    'bg-mauve-500',
    'text-olive-100',
    'border-mist-200',
    'fill-taupe-900',
    // Colour utilities beyond bg/text/border.
    'ring-offset-red-500',
    'inset-ring-blue-500',
    'inset-shadow-taupe-200',
    'text-shadow-red-500',
    'border-t-red-500',
    'bg-red-500/50'
  ])('rejects %s', (text) => expect(problemsIn(text)).not.toEqual([]));
});
