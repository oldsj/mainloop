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

  it('caps a long server reason', () => {
    const text = sendErrorText(new SendError('x'.repeat(5000), 403), false);
    expect(text).toHaveLength(300);
    expect(text.endsWith('…')).toBe(true);
  });

  it('keeps the generic line for a failure with no reason', () => {
    expect(sendErrorText(new SendError('Server error.', 500), false)).toBe(
      'Could not send the message.'
    );
  });

  it.each([400, 403, 422])('gives the server reason for a %i refusal', (status) => {
    expect(sendErrorText(new SendError('Only the owner can send here.', status), false)).toBe(
      'Only the owner can send here.'
    );
  });

  it('keeps the box note with a server reason for a refusal other than 409', () => {
    expect(sendErrorText(new SendError('Only the owner can send here.', 403), true)).toBe(
      'Only the owner can send here.' + BACK
    );
  });

  it('keeps the generic line for a 5xx, whatever its detail', () => {
    expect(sendErrorText(new SendError('database is locked', 503), true)).toBe(
      'Could not send the message.' + BACK
    );
  });

  it('gives the reason for a send with no reply', () => {
    expect(sendErrorText(new SendError("Can't reach the Mainloop backend.", 0), true)).toBe(
      "Can't reach the Mainloop backend." + BACK
    );
  });

  it('keeps the generic line when a refusal carries no reason', () => {
    expect(sendErrorText(new SendError('', 403), false)).toBe('Could not send the message.');
  });
});
