/**
 * An in-memory HITL backend for tests, built to the contract in `api/schema.gen.ts`. The Go
 * endpoints are not merged yet, so this is the only server these tests talk to.
 */
import { delay, http, HttpResponse, type HttpHandler } from 'msw';
import { API_URL } from '@/api/config';
import type { HITLMergeDetails, HITLResponse, HITLView } from '@/lib/contract';
import { decisionReceipt } from '@/test/fixtures/hitl';

const url = (path: string) => `${API_URL}${path}`;

export interface RespondCall {
  requestId: string;
  actionId: string;
  response: HITLResponse;
}

/** How the next POST /respond behaves. */
export type RespondMode =
  | 'record'
  /** Record the decision, then never answer: the browser times out with the write already made. */
  | 'record-then-hang'
  /** Never record, never answer. */
  | 'hang'
  | { status: number; detail: string };

export function createFakeHITL(initial: Record<string, HITLView> = {}) {
  const views = new Map(Object.entries(initial));
  const calls: RespondCall[] = [];
  const reads: string[] = [];
  const pages: HITLMergeDetails[] = [];
  const detailCalls: { section: string; cursor: string | null; limit: string | null }[] = [];
  let respondMode: RespondMode = 'record';
  let inFlight = 0;
  let maxInFlight = 0;

  const handlers: HttpHandler[] = [
    http.get(url('/hitl/:id'), ({ params }) => {
      const id = String(params.id);
      reads.push(id);
      const view = views.get(id);
      return view
        ? HttpResponse.json(view)
        : HttpResponse.json({ detail: 'Request not found.' }, { status: 404 });
    }),
    http.post(url('/hitl/:id/respond'), async ({ params, request }) => {
      const id = String(params.id);
      const body = (await request.json()) as { action_id: string; response: HITLResponse };
      calls.push({ requestId: id, actionId: body.action_id, response: body.response });
      inFlight++;
      maxInFlight = Math.max(maxInFlight, inFlight);
      try {
        const mode = respondMode;
        const view = views.get(id);
        if (!view) return HttpResponse.json({ detail: 'Request not found.' }, { status: 404 });
        const record = () => {
          const next: HITLView = {
            ...view,
            response: {
              ...decisionReceipt(view.request),
              action_id: body.action_id,
              response: body.response as never
            },
            transport_state: 'recorded'
          };
          views.set(id, next);
          return next;
        };
        if (mode === 'hang') return await new Promise<never>(() => {});
        if (mode === 'record-then-hang') {
          record();
          return await new Promise<never>(() => {});
        }
        if (mode !== 'record') {
          await delay(0);
          return HttpResponse.json({ detail: mode.detail }, { status: mode.status });
        }
        await delay(20);
        return HttpResponse.json(record());
      } finally {
        inFlight--;
      }
    }),
    http.get(url('/hitl/:id/merge/:proposal/details'), ({ request }) => {
      const query = new URL(request.url).searchParams;
      detailCalls.push({
        section: query.get('section') ?? '',
        cursor: query.get('cursor'),
        limit: query.get('limit')
      });
      const page = pages.shift();
      return page
        ? HttpResponse.json(page)
        : HttpResponse.json({ detail: 'No details.' }, { status: 404 });
    })
  ];

  return {
    handlers,
    calls,
    reads,
    pages,
    detailCalls,
    views,
    setRespondMode(mode: RespondMode) {
      respondMode = mode;
    },
    get maxInFlight() {
      return maxInFlight;
    }
  };
}
