/**
 * The runtime values of a generated string union, for `z.enum`. The list must name every member of
 * `U` and nothing else, so a value the contract adds or drops fails tsc here instead of silently
 * dropping rows at runtime. Usage: `z.enum(contractEnum<MergePolicy>()(['auto', 'approval']))`.
 */
export function contractEnum<U extends string>() {
  return <const L extends readonly U[]>(
    values: L & ([U] extends [L[number]] ? unknown : never)
  ): L => values;
}
