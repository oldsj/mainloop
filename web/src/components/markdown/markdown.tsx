import type { Components } from 'react-markdown';
import ReactMarkdown from 'react-markdown';
import remarkBreaks from 'remark-breaks';
import remarkGfm from 'remark-gfm';
import { cn } from '@/components/ui/cn';
import { markdownHref } from '@/lib/url';

/**
 * Message text can come from agents, so it is never trusted:
 * - raw HTML is shown as text (no `rehype-raw`, no `dangerouslySetInnerHTML`), so `<file>` in
 *   prose stays visible;
 * - only http, https and mailto links work (`markdownHref`), and they open in a new tab with
 *   noopener; any other link keeps its words and loses its target;
 * - an image shows its alt text and never loads.
 */
const components: Components = {
  a({ href, title, children }) {
    if (!href) return <>{children}</>;
    return (
      <a
        href={href}
        title={title}
        target="_blank"
        rel="noopener noreferrer"
        className="text-term-info hover:text-term-accent underline"
      >
        {children}
      </a>
    );
  },
  img: ({ alt }) => <>{alt}</>,
  p: ({ children }) => <p className="mb-sm last:mb-0">{children}</p>,
  code: ({ children }) => (
    <code className="bg-background border-border px-xs border text-xs break-words">{children}</code>
  ),
  pre: ({ children }) => (
    <pre className="bg-background border-border p-sm my-sm border break-words whitespace-pre-wrap [&_code]:border-0 [&_code]:bg-transparent [&_code]:p-0">
      {children}
    </pre>
  ),
  ul: ({ children }) => <ul className="my-sm pl-lg list-disc">{children}</ul>,
  ol: ({ children }) => <ol className="my-sm pl-lg list-decimal">{children}</ol>,
  li: ({ children }) => <li className="my-xs">{children}</li>,
  strong: ({ children }) => <strong className="text-primary font-semibold">{children}</strong>,
  em: ({ children }) => <em className="text-muted-foreground italic">{children}</em>,
  blockquote: ({ children }) => (
    <blockquote className="border-border pl-md my-sm text-muted-foreground border-l-2">
      {children}
    </blockquote>
  ),
  h1: ({ children }) => <h1 className="text-primary my-sm text-lg font-semibold">{children}</h1>,
  h2: ({ children }) => <h2 className="text-primary my-sm text-base font-semibold">{children}</h2>,
  h3: ({ children }) => <h3 className="text-primary my-sm text-sm font-semibold">{children}</h3>,
  h4: ({ children }) => <h4 className="text-primary my-sm text-sm font-semibold">{children}</h4>,
  hr: () => <hr className="border-border my-md border-0 border-t" />,
  // A wide table scrolls inside the message instead of stretching the page.
  table: ({ children }) => (
    <div className="my-sm max-w-full overflow-x-auto">
      <table className="border-collapse">{children}</table>
    </div>
  ),
  th: ({ children }) => (
    <th className="border-border px-sm py-xs bg-background text-primary border text-left">
      {children}
    </th>
  ),
  td: ({ children }) => <td className="border-border px-sm py-xs border text-left">{children}</td>
};

export interface MarkdownProps {
  children: string;
  className?: string;
}

/** Renders message text as Markdown (GFM tables, single newlines as line breaks). */
export function Markdown({ children, className }: MarkdownProps) {
  return (
    <div className={cn('min-w-0 [overflow-wrap:anywhere]', className)}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm, remarkBreaks]}
        urlTransform={markdownHref}
        components={components}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}
