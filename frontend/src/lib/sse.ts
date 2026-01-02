/**
 * SSE (Server-Sent Events) client for real-time updates
 *
 * Uses EventSource for automatic reconnection and native browser support.
 */

const API_URL = import.meta.env.VITE_API_URL || 'http://localhost:8000';

export type SSEEventType =
  | 'connected'
  | 'task:updated'
  | 'inbox:updated'
  | 'heartbeat'
  | 'log'
  | 'status'
  | 'end';

export interface SSEEvent {
  event: SSEEventType;
  data: Record<string, unknown>;
  id?: string;
}

export type SSEEventHandler = (event: SSEEvent) => void;

interface SSEClientOptions {
  onConnect?: () => void;
  onDisconnect?: () => void;
  onError?: (error: Event) => void;
  onEvent?: SSEEventHandler;
}

/**
 * Global SSE client for receiving real-time updates from the server.
 *
 * EventSource automatically handles:
 * - Reconnection with exponential backoff
 * - Last-Event-ID for resuming from where we left off
 */
export class SSEClient {
  private eventSource: EventSource | null = null;
  private handlers: Map<SSEEventType, Set<SSEEventHandler>> = new Map();
  private options: SSEClientOptions;
  private url: string;

  constructor(url: string, options: SSEClientOptions = {}) {
    this.url = url;
    this.options = options;
  }

  /**
   * Connect to the SSE endpoint.
   */
  connect(): void {
    if (this.eventSource) {
      return; // Already connected
    }

    this.eventSource = new EventSource(this.url, {
      withCredentials: false // Set to true if using cookies for auth
    });

    this.eventSource.onopen = () => {
      console.log('[SSE] Connected to', this.url);
      this.options.onConnect?.();
    };

    this.eventSource.onerror = (error) => {
      console.error('[SSE] Connection error:', error);
      this.options.onError?.(error);

      // EventSource will automatically try to reconnect
      // We just log the disconnect for now
      if (this.eventSource?.readyState === EventSource.CLOSED) {
        console.log('[SSE] Connection closed, will reconnect...');
        this.options.onDisconnect?.();
      }
    };

    // Listen for all event types we care about
    const eventTypes: SSEEventType[] = [
      'connected',
      'task:updated',
      'inbox:updated',
      'heartbeat',
      'log',
      'status',
      'end'
    ];

    for (const eventType of eventTypes) {
      this.eventSource.addEventListener(eventType, (event: MessageEvent) => {
        try {
          const data = JSON.parse(event.data);
          const sseEvent: SSEEvent = {
            event: eventType,
            data,
            id: event.lastEventId
          };

          // Call global handler
          this.options.onEvent?.(sseEvent);

          // Call type-specific handlers
          const handlers = this.handlers.get(eventType);
          if (handlers) {
            for (const handler of handlers) {
              handler(sseEvent);
            }
          }
        } catch (e) {
          console.error('[SSE] Failed to parse event:', e, event.data);
        }
      });
    }
  }

  /**
   * Disconnect from the SSE endpoint.
   */
  disconnect(): void {
    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
      console.log('[SSE] Disconnected from', this.url);
      this.options.onDisconnect?.();
    }
  }

  /**
   * Check if connected.
   */
  isConnected(): boolean {
    return this.eventSource?.readyState === EventSource.OPEN;
  }

  /**
   * Subscribe to a specific event type.
   */
  on(eventType: SSEEventType, handler: SSEEventHandler): () => void {
    if (!this.handlers.has(eventType)) {
      this.handlers.set(eventType, new Set());
    }
    this.handlers.get(eventType)!.add(handler);

    // Return unsubscribe function
    return () => {
      this.handlers.get(eventType)?.delete(handler);
    };
  }

  /**
   * Remove a handler for a specific event type.
   */
  off(eventType: SSEEventType, handler: SSEEventHandler): void {
    this.handlers.get(eventType)?.delete(handler);
  }
}

// Global SSE client instance for the main event stream
let globalClient: SSEClient | null = null;

/**
 * Get or create the global SSE client for real-time updates.
 */
export function getSSEClient(): SSEClient {
  if (!globalClient) {
    globalClient = new SSEClient(`${API_URL}/events`);
  }
  return globalClient;
}

/**
 * Connect the global SSE client.
 * Call this when the app starts.
 */
export function connectSSE(): void {
  getSSEClient().connect();
}

/**
 * Disconnect the global SSE client.
 * Call this when the app is unmounting.
 */
export function disconnectSSE(): void {
  if (globalClient) {
    globalClient.disconnect();
    globalClient = null;
  }
}

/**
 * Create an SSE client for streaming task logs.
 */
export function createTaskLogClient(taskId: string, options: SSEClientOptions = {}): SSEClient {
  return new SSEClient(`${API_URL}/tasks/${taskId}/logs/stream`, options);
}
