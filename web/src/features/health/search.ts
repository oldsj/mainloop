/**
 * The health page's filters, held in the URL. Each value is parsed at the route boundary: an
 * unknown window or provider is dropped to its default instead of failing the page. Keep the
 * window list in step with the server's accepted `hours` values.
 */
import { z } from 'zod';

export const HOUR_WINDOWS = [24, 168, 720, 8760] as const;
export type HourWindow = (typeof HOUR_WINDOWS)[number];

const optionalString = z.string().optional().catch(undefined);

export const healthSearch = z.object({
  project_id: optionalString,
  task_id: optionalString,
  session_id: optionalString,
  attempt_id: optionalString,
  provider: z.enum(['claude', 'codex']).optional().catch(undefined),
  hours: z.coerce
    .number()
    .pipe(z.number().refine((hours) => (HOUR_WINDOWS as readonly number[]).includes(hours)))
    .optional()
    .catch(undefined)
});

export type HealthSearch = z.infer<typeof healthSearch>;
