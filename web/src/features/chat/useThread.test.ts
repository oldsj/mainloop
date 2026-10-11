import { describe, expect, it } from 'vitest';
import { SendError, type RefusalCode } from '@/api/errors';
import { sendErrorText } from './useThread';

/** A 409 send refusal as the client builds it from a body with the contract's `code`. */
const coded = (code: RefusalCode, message = 'Raw server prose.') =>
  new SendError(message, 409, code);

const BACK = ' Your message is back in the box.';

describe('sendErrorText', () => {
  it.each<[RefusalCode, string]>([
    ['busy', 'Message not sent: the main has open work or changed while the request ran.'],
    ['pending', 'Message not sent: a provider switch is pending.'],
    ['unconfirmed', 'Message not sent: kagent could not confirm the main is idle.'],
    ['unresolved', 'Message not sent: a native create for the main is unresolved.'],
    [
      'agent_unavailable',
      "Message not sent: the other provider isn't installed here. Nothing changed."
    ]
  ])('%s shows its own notice, not the server prose', (code, expected) => {
    expect(sendErrorText(coded(code), false)).toBe(expected);
  });

  it('keeps the box note when the message was restored', () => {
    expect(sendErrorText(coded('busy'), true)).toBe(
      'Message not sent: the main has open work or changed while the request ran.' + BACK
    );
  });

  it.each([
    ['an unknown code', new SendError('Raw server prose.', 409, 'mystery' as RefusalCode)],
    ['no code', new SendError('Raw server prose.', 409)]
  ])('falls back to the detail for %s', (_label, error) => {
    expect(sendErrorText(error, false)).toBe('Raw server prose.');
  });

  it('shows the detail with the box note when there is no code', () => {
    expect(sendErrorText(new SendError('Raw server prose.', 409), true)).toBe(
      'Raw server prose.' + BACK
    );
  });

  it('keeps the generic line for a failure with no reason', () => {
    expect(sendErrorText(new SendError('Server error.', 500), false)).toBe(
      'Could not send the message.'
    );
  });
});
