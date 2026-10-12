import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import { describe, expect, it } from 'vitest';
import { server } from '@/test/msw/server';
import { $api, fetchClient } from './client';
import { API_URL } from './config';
import { ApiError } from './errors';

// These go through the real client and middleware, with MSW answering at the network layer. They
// guard against a client that captured `fetch` before MSW patched it, or a relative base URL.
describe('the real API client', () => {
  it('reaches MSW through fetchClient', async () => {
    const { data, error } = await fetchClient.GET('/health');
    expect(error).toBeUndefined();
    expect(data).toEqual({ status: 'healthy', database: 'ok' });
  });

  it('maps an error response to ApiError through the real middleware', async () => {
    server.use(
      http.get(`${API_URL}/projects`, () => HttpResponse.json({ detail: 'Nope' }, { status: 403 }))
    );
    const error = await fetchClient.GET('/projects').catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: 403, message: 'Nope' });
  });

  it('serves $api.useQuery', async () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    const { result } = renderHook(() => $api.useQuery('get', '/health'), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.status).toBe('healthy');
  });
});
