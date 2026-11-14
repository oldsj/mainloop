import { describe, expect, it } from 'vitest';
import { barStatus, barStatusDot, type LiveStatus } from './barStatus';

describe('barStatus', () => {
  it('uses the thread status alone when there is no live view', () => {
    expect(barStatus('ready', null)).toBe('ready');
    expect(barStatus('idle', null)).toBe('idle');
    expect(barStatus('working', null)).toBe('working');
    expect(barStatus('failed', null)).toBe('last message failed');
    expect(barStatus('unconfirmed', null)).toBe('last message unconfirmed');
    expect(barStatus('unreachable', null)).toBe('unreachable');
  });

  it('says asleep, not ready or idle, for a sleeping live session', () => {
    expect(barStatus('ready', 'asleep')).toBe('asleep');
    expect(barStatus('idle', 'asleep')).toBe('asleep');
  });

  it('puts unreachable above everything the live view says', () => {
    for (const live of ['needs you', 'working', 'ready', 'asleep'] as LiveStatus[])
      expect(barStatus('unreachable', live)).toBe('unreachable');
  });

  it('puts needs you and a running turn above a failed last message', () => {
    expect(barStatus('failed', 'needs you')).toBe('needs you');
    expect(barStatus('failed', 'working')).toBe('working');
    expect(barStatus('unconfirmed', 'starting')).toBe('starting');
  });

  it('never shows a failed or unconfirmed last message as ready, asleep or connecting', () => {
    for (const live of [
      'ready',
      'asleep',
      'paused',
      'connecting…',
      'reconnecting…'
    ] as LiveStatus[]) {
      expect(barStatus('failed', live)).toBe('last message failed');
      expect(barStatus('unconfirmed', live)).toBe('last message unconfirmed');
    }
  });

  it('shows a delivery in flight as working while the live view is not attached', () => {
    expect(barStatus('working', 'reconnecting…')).toBe('working');
    expect(barStatus('working', 'ready')).toBe('working');
  });

  it('otherwise says what the live channel is doing', () => {
    expect(barStatus('ready', 'ready')).toBe('ready');
    expect(barStatus('ready', 'waking…')).toBe('waking…');
    expect(barStatus('ready', 'unavailable')).toBe('unavailable');
    expect(barStatus(null, 'paused')).toBe('paused');
  });
});

describe('barStatusDot', () => {
  it('gives each meaning one colour', () => {
    expect(barStatusDot('needs you')).toContain('bg-status-attention');
    expect(barStatusDot('working')).toContain('bg-status-working');
    expect(barStatusDot('last message failed')).toContain('bg-status-failed');
    expect(barStatusDot('asleep')).toBe('bg-status-idle');
    expect(barStatusDot('ready')).toBe('bg-term-green');
  });
});
