/**
 * The reading width of a conversation: the transcript and its composer are centred on it, so a line
 * stays near 100 characters however wide the window is. The scroller around them stays full width.
 */
export const MEASURE = 'mx-auto w-full max-w-measure';

/**
 * The column a page sits in: the same width as a conversation, centred, with the page's padding. A
 * page that is a form or a list of sections uses it, so the title is in the same place on each and
 * a field is never the full width of a wide window.
 */
export const PAGE = `${MEASURE} p-md flex min-w-0 flex-col`;
