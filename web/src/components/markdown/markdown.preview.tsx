import { Markdown } from './markdown';

const SAMPLE = `# Heading

Text with **strong**, _emphasis_, \`inline code\` and a [safe link](https://example.com "Example").
A single newline is a line break.

- list item
- another item

> a quote

\`\`\`
a code block
\`\`\`

| a | b |
| - | - |
| 1 | 2 |

Not links: [script](javascript:alert(1)) and <b>raw html</b>. An image: ![alt text](https://example.com/x.png)`;

/** Every supported element, and the inputs that must stay inert. */
export default function MarkdownPreview() {
  return (
    <div className="p-md">
      <Markdown>{SAMPLE}</Markdown>
    </div>
  );
}
