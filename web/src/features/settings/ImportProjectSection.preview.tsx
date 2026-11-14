import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { useState } from 'react';
import { ImportProjectSection } from './ImportProjectSection';

/** The empty form. The section reads nothing until the owner imports, so the preview makes no request. */
export default function ImportProjectSectionPreview() {
  const [client] = useState(() => new QueryClient());
  return (
    <QueryClientProvider client={client}>
      <div className="p-md">
        <ImportProjectSection />
      </div>
    </QueryClientProvider>
  );
}
