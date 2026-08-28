package absdb

import (
	"bytes"
	"testing"

	"github.com/cwbudde/go-absolute-database/internal/zlib1"
)

// TestZlib1ReproducesEveryCorpusStream is the evidence behind the claim that
// internal/zlib1 can stand in for the engine's compressor: every compressed
// internal file in the corpus — schema (type 8), table info (9) and catalog
// (6) — must come back byte for byte when its decompressed content is fed
// through Compress.
//
// This is the gate the schema operations were blocked on. Reading a rewritten
// stream back correctly is not sufficient evidence: a stream compressed with
// Go's own encoder inflates to the right bytes and is still not the file the
// engine writes, and only a comparison against the engine's own output can tell
// the two apart.
func TestZlib1ReproducesEveryCorpusStream(t *testing.T) {
	var checked, mismatched int

	eachCompressedInternalFile(t, func(p internalFilePage) {
		stream := p.stream()
		if stream == nil {
			t.Errorf("%s page %d: internal file header points outside the page",
				p.fixture, p.pageNo)

			return
		}

		raw, err := inflateLimited(stream, p.decompressed, internalFileInflateBounds)
		if err != nil {
			t.Errorf("%s page %d: inflating: %v", p.fixture, p.pageNo, err)

			return
		}

		checked++

		got := zlib1.Compress(raw)
		if bytes.Equal(got, stream) {
			return
		}

		mismatched++

		t.Errorf("%s page %d: recompressing %d bytes gave %d bytes, engine wrote %d (first difference at %d)",
			p.fixture, p.pageNo, len(raw), len(got), len(stream), firstDiffOffset(got, stream))
	})

	if checked == 0 {
		t.Skip("no compressed internal files in the fixtures")
	}

	t.Logf("re-compressed %d internal file streams; %d differed from the engine's bytes",
		checked, mismatched)
}

// firstDiffOffset reports the offset of the first differing byte, or -1 when
// the two slices share a prefix and differ only in length.
func firstDiffOffset(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}

	return -1
}
