/**
 * Data hooks for the inbox. Reads and writes go through the generated client, and every cache key
 * comes from `keys`, so SSE invalidation reaches these queries. Writes update the cache in place
 * where the outcome is known, and re-read it where the server has the final say.
 */
import { useQueryClient } from '@tanstack/react-query';
import { $api, keys } from '@/api';
import { type ActivityItem, withoutActivityItem } from './model';

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
 * Dismisses an unavailable input notice, named by its Activity row's `queue_item_id`. The row
 * leaves Activity at once. A refused dismiss puts the previous rows back, and the caller shows the
 * reason. Either way both lists are re-read, so the owner sees the server's final state.
 */
export function useDismissQueueItem() {
  const queryClient = useQueryClient();
  return $api.useMutation('post', '/queue/{item_id}/dismiss', {
    onMutate: async ({ params }) => {
      const queueItemId = params.path.item_id;
      await queryClient.cancelQueries({ queryKey: keys.activity() });
      const previous = queryClient.getQueriesData<ActivityItem[]>({ queryKey: keys.activity() });
      queryClient.setQueriesData<ActivityItem[]>({ queryKey: keys.activity() }, (list) =>
        list ? withoutActivityItem(list, queueItemId) : list
      );
      return { previous };
    },
    onError: (_error, _variables, context) => {
      for (const [key, list] of context?.previous ?? []) {
        queryClient.setQueryData(key, list);
      }
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: keys.attention() });
      void queryClient.invalidateQueries({ queryKey: keys.activity() });
    }
  });
}
