import { $api } from '@/api';

/** The screenshots a report shows: preview_screenshot captures of the task's workspace. */
export function ReportScreenshots({ taskId, ids }: { taskId: string; ids: string[] }) {
  if (ids.length === 0) return null;
  return (
    <ul className="gap-sm flex flex-wrap" aria-label="Screenshots">
      {ids.map((id) => (
        <li key={id}>
          <ReportScreenshot taskId={taskId} id={id} />
        </li>
      ))}
    </ul>
  );
}

function ReportScreenshot({ taskId, id }: { taskId: string; id: string }) {
  const shot = $api.useQuery(
    'get',
    '/tasks/{task_id}/screenshots/{screenshot_id}',
    { params: { path: { task_id: taskId, screenshot_id: id } } },
    // A screenshot never changes once stored.
    { staleTime: Infinity, gcTime: 10 * 60_000 }
  );
  if (shot.isPending) {
    return (
      <div
        className="border-border text-muted-foreground flex h-40 w-24 items-center justify-center border text-xs"
        role="status"
      >
        Loading…
      </div>
    );
  }
  if (shot.isError || !shot.data) {
    return <p className="text-muted-foreground text-xs">Screenshot unavailable</p>;
  }
  const s = shot.data;
  const caption = `${s.viewport} · port ${s.port} ${s.path}`;
  if (s.data === undefined) {
    // The image expired; the screenshot's facts stay.
    return (
      <figure className="gap-2xs flex flex-col">
        <div className="border-border text-muted-foreground flex h-40 w-24 items-center justify-center border text-xs">
          Expired
        </div>
        <figcaption className="text-muted-foreground text-2xs break-all">{caption}</figcaption>
      </figure>
    );
  }
  return (
    <figure className="gap-2xs flex flex-col">
      <img
        src={`data:${s.media_type};base64,${s.data}`}
        alt={`Preview screenshot, ${caption}`}
        width={s.width}
        height={s.height}
        loading="lazy"
        className={
          s.viewport === 'mobile'
            ? 'border-border h-auto max-h-[28rem] w-auto max-w-[13rem] border'
            : 'border-border h-auto w-full max-w-xl border'
        }
      />
      <figcaption className="text-muted-foreground text-2xs break-all">{caption}</figcaption>
    </figure>
  );
}
