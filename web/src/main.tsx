import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider } from '@tanstack/react-router';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { createQueryClient } from './api/queryClient';
import { createAppRouter } from './router';
import './styles/index.css';

async function enableMocks() {
  // `VITE_MSW=1 pnpm dev` serves the API from fixtures (src/test/msw), with no backend. Add
  // `VITE_MSW_SEED=review` (or open with `?seed=review`) for browser-review data: src/test/msw/review-seed.ts.
  if (import.meta.env.DEV && import.meta.env.VITE_MSW === '1') {
    const { worker } = await import('./test/msw/browser');
    await worker.start({ onUnhandledRequest: 'bypass', quiet: true });
  }
}

const queryClient = createQueryClient();
const router = createAppRouter(queryClient);

void enableMocks().then(() => {
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </StrictMode>
  );
});
