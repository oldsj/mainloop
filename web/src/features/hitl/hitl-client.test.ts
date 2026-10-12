import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { createHITLChannel, type HITLDraft } from '@/lib/hitl';
import { server } from '@/test/msw/server';
import { hitlView } from '@/test/fixtures/hitl';
import { createFakeHITL } from './handlers';
import { createHITLSend, readHITL, RESPOND_TIMEOUT_MS } from './hitl-client';

const approveAll: HITLDraft = {
  decisions: { a: 'approve', b: 'reject' },
  reasons: { b: 'Keep tests' },
  answers: []
};

let fake: ReturnType<typeof createFakeHITL>;
let id = 0;
let requestId: string;

/** A channel per test with a short write deadline; the shared singleton keeps the real 90 s. */
function channel(timeoutMs = 60) {
  return createHITLChannel(readHITL, createHITLSend(timeoutMs));
}

beforeEach(() => {
  requestId = `request-${++id}`;
  fake = createFakeHITL({ [requestId]: hitlView(requestId) });
  server.use(...fake.handlers);
});
afterEach(() => server.resetHandlers());

describe('hitl write path', () => {
  it('waits 90 seconds for the owner write before locking', () => {
    expect(RESPOND_TIMEOUT_MS).toBe(90_000);
  });

  it('records a decision and shows the receipt', async () => {
    const c = channel();
    await c.refresh(requestId);
    await c.respond(requestId, approveAll);
    let state = { view: null as unknown };
    c.subscribe(requestId, (s) => (state = s));
    expect(fake.calls).toHaveLength(1);
    expect(fake.calls[0]?.response).toMatchObject({ type: 'tool_approval_response' });
    expect((state as { view: { response: unknown } }).view.response).not.toBeNull();
  });

  it('allows one submit at a time: a second call while busy sends nothing', async () => {
    const c = channel();
    await c.refresh(requestId);
    await Promise.all([
      c.respond(requestId, approveAll),
      c.respond(requestId, approveAll),
      c.respond(requestId, approveAll)
    ]);
    expect(fake.calls).toHaveLength(1);
    expect(fake.maxInFlight).toBe(1);
  });

  it('locks as uncertain on timeout, re-reads, and does not resubmit', async () => {
    fake.setRespondMode('hang');
    const c = channel(40);
    let latest = { uncertain: false, error: null as string | null, stale: false, busy: false };
    c.subscribe(requestId, (s) => (latest = s));
    await c.refresh(requestId);
    await c.respond(requestId, approveAll);
    expect(latest.uncertain).toBe(true);
    expect(latest.error).toMatch(/do not submit again/);
    expect(fake.calls).toHaveLength(1);
    // The re-read found no recorded decision, so the lock stays and nothing is resent.
    await c.respond(requestId, approveAll);
    await c.respond(requestId, approveAll);
    expect(fake.calls).toHaveLength(1);
    expect(latest.uncertain).toBe(true);
  });

  it('clears the lock once the re-read shows the decision was recorded', async () => {
    fake.setRespondMode('record-then-hang');
    const c = channel(40);
    let latest = { uncertain: false, view: null as { response: unknown } | null };
    c.subscribe(requestId, (s) => (latest = s as never));
    await c.refresh(requestId);
    await c.respond(requestId, approveAll);
    expect(fake.calls).toHaveLength(1);
    expect(latest.uncertain).toBe(false);
    expect(latest.view?.response).not.toBeNull();
  });

  it('shows a 4xx detail and refreshes without locking', async () => {
    fake.setRespondMode({ status: 409, detail: 'This request was already answered.' });
    const c = channel();
    let latest = { uncertain: true, error: null as string | null };
    c.subscribe(requestId, (s) => (latest = s));
    await c.refresh(requestId);
    const readsBefore = fake.reads.length;
    await c.respond(requestId, approveAll);
    expect(latest.uncertain).toBe(false);
    expect(latest.error).toBe('This request was already answered. Refreshing the request.');
    expect(fake.reads.length).toBeGreaterThan(readsBefore);
  });

  it('turns a 503 with the write gate off into a disabled state, not a lock', async () => {
    fake.views.set(requestId, hitlView(requestId));
    fake.setRespondMode({ status: 503, detail: 'Owner writes are disabled.' });
    const c = channel();
    let latest = { uncertain: true, error: null as string | null };
    c.subscribe(requestId, (s) => (latest = s));
    await c.refresh(requestId);
    // The gate closes after the page loaded; the server still reports it open until re-read.
    fake.views.set(requestId, hitlView(requestId, { writes_enabled: false }));
    await c.respond(requestId, approveAll);
    expect(latest.uncertain).toBe(false);
    expect(latest.error).toMatch(/Responding is disabled on the server\. Nothing was recorded\./);
    expect(fake.calls).toHaveLength(1);
  });

  it('keeps a 503 as uncertain when the server does not prove the gate is off', async () => {
    fake.setRespondMode({ status: 503, detail: 'Bad gateway.' });
    const c = channel();
    let latest = { uncertain: false, error: null as string | null };
    c.subscribe(requestId, (s) => (latest = s));
    await c.refresh(requestId);
    await c.respond(requestId, approveAll);
    expect(latest.uncertain).toBe(true);
    expect(latest.error).toMatch(/do not submit again/);
  });
});
