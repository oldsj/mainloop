/**
 * Markdown rendering for chat messages.
 *
 * Message text can come from agents, so raw HTML in it is shown as text (never injected), and only
 * http(s)/mailto links are made clickable. Angle brackets in prose (`<file>`) therefore stay visible.
 */
import { Marked, type Tokens } from 'marked';

const escapeHtml = (s: string) =>
  s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

const md = new Marked({ breaks: true, gfm: true });
md.use({
  renderer: {
    html: ({ text }: Tokens.HTML | Tokens.Tag) => escapeHtml(text),
    link({ href, title, tokens }: Tokens.Link) {
      const text = this.parser.parseInline(tokens);
      if (!/^(https?:|mailto:)/i.test(href)) return text;
      const t = title ? ` title="${escapeHtml(title)}"` : '';
      return `<a href="${escapeHtml(href)}"${t} target="_blank" rel="noopener noreferrer">${text}</a>`;
    },
    image: ({ text }: Tokens.Image) => escapeHtml(text)
  }
});

export function renderMarkdown(text: string): string {
  return md.parse(text) as string;
}
