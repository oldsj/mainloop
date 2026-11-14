# Markdown text

Source: `docs/specs/chat.md`.

## Purpose

Renders message text, which agents write and so cannot be trusted. Used by chat messages and anywhere else agent text is shown as formatted text.

## Rules

- Raw HTML is shown as text. `<file>` in prose stays visible and nothing is injected.
- Only `http`, `https` and `mailto` links work. They open in a new tab with `rel="noopener noreferrer"`, and keep their title. Any other link (`javascript:`, `data:`, a relative path) keeps its words and loses its target.
- An image shows its alt text and never loads.
- GitHub-flavoured Markdown: tables, task lists, strikethrough and bare-URL links. A single newline is a line break.
- A wide table or long unbroken token stays inside the message: tables scroll sideways, long words wrap.

## Tokens

- Text `text-foreground`. Strong text, headings and table headings are `text-foreground` at `font-semibold`: weight marks them, not colour.
- Links are `text-primary` and underlined, `text-foreground` on hover. The accent colour in prose means a link and nothing else.
- Emphasis and quotes `text-muted-foreground`.
- Code and tables use `bg-background` with `border-border`. Radius `rounded-none`.

## Don't

- Don't render message text with `dangerouslySetInnerHTML`.
- Don't add an HTML plugin to the renderer.
