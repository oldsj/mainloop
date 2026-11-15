package sessions

import "github.com/oldsj/mainloop/server/internal/redact"

// detailMax bounds a delivery's detail. The detail is shown to the owner next to the message, and
// error text from kagent, the harness or the stack is untrusted input.
const detailMax = 300

// safeDetail returns text as one line, bounded, with best-effort credential redaction. It returns
// "" when nothing is left.
func safeDetail(text string) string { return redact.Line(text, detailMax) }
