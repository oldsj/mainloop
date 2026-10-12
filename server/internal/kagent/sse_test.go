package kagent

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func collect(t *testing.T, in string) ([]string, error) {
	t.Helper()
	var got []string
	err := readSSE(strings.NewReader(in), func(data []byte) bool {
		got = append(got, string(data))
		return true
	})
	return got, err
}

func TestReadSSE(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"two events", "data: a\n\ndata: b\n\n", []string{"a", "b"}},
		{"crlf", "data: a\r\n\r\ndata: b\r\n\r\n", []string{"a", "b"}},
		{"no space after colon", "data:a\n\n", []string{"a"}},
		{"multi-line data is joined", "data: a\ndata: b\n\n", []string{"a\nb"}},
		{"id event and comments are ignored", ": hi\nid: 1\nevent: x\ndata: a\n\n", []string{"a"}},
		{"final event without a blank line", "data: a\n\ndata: b", []string{"a", "b"}},
		{"blank lines alone", "\n\n\n", nil},
		{"empty", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := collect(t, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") || len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadSSEStopsWhenTheConsumerStops(t *testing.T) {
	n := 0
	err := readSSE(strings.NewReader("data: a\n\ndata: b\n\n"), func([]byte) bool { n++; return false })
	if err != nil || n != 1 {
		t.Fatalf("err %v, calls %d", err, n)
	}
}

func TestReadSSELongLine(t *testing.T) {
	long := strings.Repeat("x", 200<<10)
	got, err := collect(t, "data: "+long+"\n\n")
	if err != nil || len(got) != 1 || got[0] != long {
		t.Fatalf("err %v, got %d events", err, len(got))
	}
}

func TestReadSSEOversizedEvent(t *testing.T) {
	line := "data: " + strings.Repeat("x", 1<<20) + "\n"
	_, err := collect(t, strings.Repeat(line, 17)+"\n")
	if !errors.Is(err, errEventTooLarge) {
		t.Fatalf("got %v", err)
	}
}

type brokenReader struct{ sent bool }

func (b *brokenReader) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.ErrUnexpectedEOF
	}
	b.sent = true
	return copy(p, "data: a\n\ndata: b"), nil
}

func TestReadSSEReportsABrokenBodyAndDropsTheUnfinishedEvent(t *testing.T) {
	var got []string
	err := readSSE(&brokenReader{}, func(d []byte) bool { got = append(got, string(d)); return true })
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v", err)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("got %q", got)
	}
}
