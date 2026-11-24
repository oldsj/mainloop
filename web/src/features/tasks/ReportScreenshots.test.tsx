import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { API_URL } from '@/api/config';
import { server } from '@/test/msw/server';
import { ReportScreenshots } from './ReportScreenshots';

afterEach(async () => {
  await connection.check();
});

vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('./test/fastReads');
  return withFastReads(await importOriginal());
});

const url = (path: string) => `${API_URL}${path}`;

function renderShots(ids: string[]) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <ReportScreenshots taskId="task-1" ids={ids} />
    </QueryClientProvider>
  );
}

describe('ReportScreenshots', () => {
  it("shows each of the report's screenshots with what it captured", async () => {
    server.use(
      http.get(url('/tasks/:task_id/screenshots/:screenshot_id'), ({ params }) =>
        params.screenshot_id === 'shot-1'
          ? HttpResponse.json({
              id: 'shot-1',
              media_type: 'image/png',
              data: 'AQIDBA==',
              viewport: 'mobile',
              port: 3000,
              path: '/menu',
              width: 780,
              height: 1688,
              created_at: '2026-01-01T10:00:00Z'
            })
          : HttpResponse.json({ detail: 'Screenshot not found' }, { status: 404 })
      )
    );
    renderShots(['shot-1', 'shot-gone']);

    const image = await screen.findByRole('img', {
      name: 'Preview screenshot, mobile · port 3000 /menu'
    });
    expect(image).toHaveAttribute('src', 'data:image/png;base64,AQIDBA==');
    expect(await screen.findByText('Screenshot unavailable')).toBeInTheDocument();
  });

  it('says an expired screenshot expired, with what it captured', async () => {
    server.use(
      http.get(url('/tasks/:task_id/screenshots/:screenshot_id'), () =>
        HttpResponse.json({
          id: 'shot-old',
          media_type: 'image/png',
          expired_at: '2026-01-08T10:00:00Z',
          viewport: 'desktop',
          port: 3000,
          path: '/menu',
          width: 1440,
          height: 900,
          created_at: '2026-01-01T10:00:00Z'
        })
      )
    );
    renderShots(['shot-old']);

    expect(await screen.findByText('Expired')).toBeInTheDocument();
    expect(screen.getByText('desktop · port 3000 /menu')).toBeInTheDocument();
    expect(screen.queryByRole('img')).not.toBeInTheDocument();
  });

  it('renders nothing for a report without screenshots', () => {
    const { container } = renderShots([]);
    expect(container).toBeEmptyDOMElement();
  });
});
