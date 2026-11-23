package webfetch

import (
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// skipped are elements whose content is never text a reader sees.
var skipped = map[atom.Atom]bool{
	atom.Head: true, atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Svg: true,
	atom.Template: true, atom.Iframe: true, atom.Object: true, atom.Canvas: true, atom.Select: true,
}

// blocks start and end a line.
var blocks = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true, atom.Main: true,
	atom.Header: true, atom.Footer: true, atom.Nav: true, atom.Aside: true, atom.Blockquote: true,
	atom.Ul: true, atom.Ol: true, atom.Dl: true, atom.Dt: true, atom.Dd: true, atom.Table: true,
	atom.Tr: true, atom.Figure: true, atom.Figcaption: true, atom.Form: true, atom.Details: true,
	atom.Summary: true, atom.Address: true, atom.Hr: true,
}

var headings = map[atom.Atom]string{
	atom.H1: "# ", atom.H2: "## ", atom.H3: "### ", atom.H4: "#### ", atom.H5: "##### ", atom.H6: "###### ",
}

var (
	spaces    = regexp.MustCompile(`[ \t\r\f\v\x{00a0}]+`)
	blankRuns = regexp.MustCompile(`\n{3,}`)
)

// HTMLText renders an HTML document as plain text: its title, and its visible text with headings
// marked `#`, list items `-`, blocks on their own lines and links as `[text](url)` against base.
// Scripts, styles and other non-text elements are dropped.
func HTMLText(doc string, base *url.URL) (title, text string) {
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return "", doc
	}
	w := &textWriter{base: base}
	w.walk(root, false)
	lines := strings.Split(w.b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	out := blankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(spaces.ReplaceAllString(w.title, " ")), strings.TrimSpace(out)
}

type textWriter struct {
	b     strings.Builder
	base  *url.URL
	title string
}

// newline ends the current line unless it is already ended.
func (w *textWriter) newline() {
	s := w.b.String()
	if s != "" && !strings.HasSuffix(s, "\n") {
		w.b.WriteByte('\n')
	}
}

func (w *textWriter) text(s string, pre bool) {
	if pre {
		w.b.WriteString(s)
		return
	}
	s = spaces.ReplaceAllString(strings.ReplaceAll(s, "\n", " "), " ")
	cur := w.b.String()
	if cur == "" || strings.HasSuffix(cur, "\n") || strings.HasSuffix(cur, " ") {
		s = strings.TrimLeft(s, " ")
	}
	w.b.WriteString(s)
}

func (w *textWriter) walk(n *html.Node, pre bool) {
	switch n.Type {
	case html.TextNode:
		w.text(n.Data, pre)
		return
	case html.ElementNode:
		if n.DataAtom == atom.Title && w.title == "" {
			w.title = textOf(n)
			return
		}
		if skipped[n.DataAtom] {
			// The title lives in head; look for it there without rendering head.
			if n.DataAtom == atom.Head {
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == html.ElementNode && c.DataAtom == atom.Title && w.title == "" {
						w.title = textOf(c)
					}
				}
			}
			return
		}
		switch {
		case n.DataAtom == atom.Br:
			w.b.WriteByte('\n')
			return
		case n.DataAtom == atom.A:
			w.link(n, pre)
			return
		case n.DataAtom == atom.Pre:
			w.newline()
			w.b.WriteByte('\n')
			w.children(n, true)
			w.newline()
			w.b.WriteByte('\n')
			return
		case headings[n.DataAtom] != "":
			w.newline()
			w.b.WriteString("\n" + headings[n.DataAtom])
			w.children(n, pre)
			w.b.WriteByte('\n')
			return
		case n.DataAtom == atom.Li:
			w.newline()
			w.b.WriteString("- ")
			w.children(n, pre)
			w.newline()
			return
		case n.DataAtom == atom.Td || n.DataAtom == atom.Th:
			w.children(n, pre)
			w.b.WriteString(" | ")
			return
		case blocks[n.DataAtom]:
			w.newline()
			w.children(n, pre)
			w.newline()
			return
		}
	}
	w.children(n, pre)
}

func (w *textWriter) children(n *html.Node, pre bool) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c, pre)
	}
}

// link writes an anchor as [text](url) when its target is an http(s) URL, else as its text.
func (w *textWriter) link(n *html.Node, pre bool) {
	label := strings.TrimSpace(spaces.ReplaceAllString(strings.ReplaceAll(textOf(n), "\n", " "), " "))
	href := ""
	for _, a := range n.Attr {
		if a.Key == "href" {
			href = strings.TrimSpace(a.Val)
		}
	}
	target := resolve(w.base, href)
	if label == "" || target == "" {
		w.children(n, pre)
		return
	}
	w.text(" ", pre)
	w.text("["+label+"]("+target+")", pre)
	w.text(" ", pre)
}

func resolve(base *url.URL, href string) string {
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}

// textOf is the text inside n, skipping non-text elements.
func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		if n.Type == html.ElementNode && skipped[n.DataAtom] && n.DataAtom != atom.Head {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
