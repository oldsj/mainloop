import { setupWorker } from 'msw/browser';
import { createChatBackend } from '@/features/chat/handlers';
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
  ...localChat.handlers,
  ...localHITL.handlers
);
