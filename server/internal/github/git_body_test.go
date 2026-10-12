package github

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

func TestGitBodyReusesStorageAcrossReads(t *testing.T) {
	input := bytes.Repeat([]byte("PACK"), 1<<20)
	output := make([]byte, 4096)
	allocs := testing.AllocsPerRun(3, func() {
		dog := time.AfterFunc(time.Hour, func() {})
		body := newGitBody(io.NopCloser(bytes.NewReader(input)), dog, "fixture-token-for-reflection-checks", func() {})
		defer func() { _ = body.Close() }()
		total := 0
		for {
			n, err := body.Read(output)
			if !bytes.Equal(output[:n], input[total:total+n]) {
				t.Fatal("reusing storage changed response bytes")
			}
			total += n
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if total != len(input) {
			t.Fatalf("read %d of %d bytes", total, len(input))
		}
	})
	// 128 upstream reads should cost only setup/growing buffers, not an allocation per read.
	if allocs > 40 {
		t.Fatalf("%.0f allocations for 128 reads, want at most 40", allocs)
	}
}
