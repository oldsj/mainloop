/**
 * A page's reading width: the content is centred on it, so a line stays near 100 characters however
 * wide the window is.
 */
export const MEASURE = 'mx-auto w-full max-w-measure';

/**
 * The width of a conversation: the transcript and its composer fill their pane from the left edge,
 * as a terminal does, and stop spreading at about 180 characters on a very wide window. The scroller
 * around them stays full width.
 */
export const CONVERSATION = 'w-full max-w-terminal';

/**
 * The column a page sits in: the reading width, centred, with the page's padding. A
 * page that is a form or a list of sections uses it, so the title is in the same place on each and
 * a field is never the full width of a wide window.
 */
export const PAGE = `${MEASURE} p-md flex min-w-0 flex-col`;
