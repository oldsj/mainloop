/** Stand-in a feature slice replaces. */
export function RoutePlaceholder({
  title,
  slice,
  testId,
  detail
}: {
  title: string;
  slice: string;
  testId?: string;
  detail?: string;
}) {
  return (
    <section data-testid={testId} className="p-md">
      <h2 className="text-foreground text-sm font-semibold">{title}</h2>
      <p className="text-muted-foreground text-xs">{detail ?? `Not built yet (${slice}).`}</p>
    </section>
  );
}
