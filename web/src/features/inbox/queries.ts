/**
 * Data hooks for the inbox. Reads and writes go through the generated client, and every cache key
 * comes from `keys`, so SSE invalidation reaches these queries. Writes update attention in place
 * where the outcome is known, and re-read it where the server has the final say.
 */
import { useQueryClient } from '@tanstack/react-query';
import { $api, ApiError, keys } from '@/api';
import { type AttentionSnapshot, withoutQueueItem } from './model';

export function useAttention() {
  return $api.useQuery('get', '/attention');
}

export function useActivity() {
  return $api.useQuery('get', '/activity');
}

/** Sends a free-text or option answer to a queue item, then re-reads the inbox. */
export function useRespondToQueueItem() {
  const queryClient = useQueryClient();
  return $api.useMutation('post', '/queue/{item_id}/respond', {
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: keys.attention() });
      void queryClient.invalidateQueries({ queryKey: keys.activity() });
    }
  });
}

/**
 * Dismisses an unavailable input notice. The row leaves Needs you at once. A refused dismiss puts
 * the previous list back; a 409 also re-reads, because the server's state has moved on.
 */
export function useDismissQueueItem() {
  const queryClient = useQueryClient();
  return $api.useMutation('post', '/queue/{item_id}/dismiss', {
    onMutate: async ({ params }) => {
      const queueItemId = params.path.item_id;
      await queryClient.cancelQueries({ queryKey: keys.attention() });
      const previous = queryClient.getQueriesData<AttentionSnapshot>({
        queryKey: keys.attention()
      });
      queryClient.setQueriesData<AttentionSnapshot>({ queryKey: keys.attention() }, (snapshot) =>
        snapshot ? withoutQueueItem(snapshot, queueItemId) : snapshot
      );
      return { previous };
    },
    onError: (error, _variables, context) => {
      for (const [key, snapshot] of context?.previous ?? []) {
        queryClient.setQueryData(key, snapshot);
      }
      if (error instanceof ApiError && error.status === 409) {
        void queryClient.invalidateQueries({ queryKey: keys.attention() });
      }
    }
  });
}
