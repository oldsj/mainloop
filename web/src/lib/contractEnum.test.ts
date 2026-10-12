import { describe, expect, it } from 'vitest';
import { contractEnum } from './contractEnum';

describe('contractEnum', () => {
  it('returns the values in the order given', () => {
    expect(contractEnum<'a' | 'b'>()(['b', 'a'])).toEqual(['b', 'a']);
  });

  // The checks below are compile-time. tsc -b fails if either @ts-expect-error line stops erroring.
  it('rejects a list that omits a generated member', () => {
    // @ts-expect-error 'b' is a generated member, and the list leaves it out
    contractEnum<'a' | 'b'>()(['a']);
  });

  it('rejects a list that names a value outside the generated union', () => {
    // @ts-expect-error 'c' is not a generated member
    contractEnum<'a' | 'b'>()(['a', 'b', 'c']);
  });
});
