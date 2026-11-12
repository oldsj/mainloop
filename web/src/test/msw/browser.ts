import { setupWorker } from 'msw/browser';
import { createChatBackend } from '@/features/chat/handlers';
import { createFakeRunner } from '@/features/live/fakeRunner';
import { createFakeHITL } from '@/features/hitl/handlers';
import { hitlView } from '@/test/fixtures/hitl';
import { baseHandlers } from './handlers';
import { reviewSeed, reviewSeedRequested } from './review-seed';

// The local stack only: a sent message's turn ends after 1.5 s, and `?hitl=request` opens a pending
// approval card. Tests never load this file, so their default chat backend has no turn timer. The
// review seed, when asked for, goes first so its data wins over the empty defaults.
const seed = reviewSeedRequested() ? reviewSeed() : null;
const localChat = createChatBackend({}, { autoEndTurnMs: 1500 });
const localHITL = createFakeHITL(seed?.requests ?? { request: hitlView('request') });

export const worker = setupWorker(
  ...(seed?.handlers ?? []),
  ...baseHandlers,
  // Every session's live channel: open one with `?live=1`. Its input goes to the fake runner for a
  // session whose live view attached, and to the chat backend otherwise.
  ...createFakeRunner({
    step: 400,
    owns: (id, runner) => runner.attaches.some((url) => url.includes(`/sessions/${id}/live`))
  }).handlers,
  ...localChat.handlers,
  ...localHITL.handlers
);
