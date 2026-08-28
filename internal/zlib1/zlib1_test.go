package zlib1

import (
	"bytes"
	"compress/zlib"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenDir holds the oracle: for every <case>.in, the file <case>.z is exactly
// what the C zlib library (1.2.13) produced for it at compression level 1.
const goldenDir = "../../testdata/zlib1"

// goldenCases returns the base name of every golden pair, skipping the test
// when the directory is absent.
func goldenCases(t *testing.T) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(goldenDir, "*.in"))
	if err != nil {
		t.Fatalf("globbing %s: %v", goldenDir, err)
	}

	if len(matches) == 0 {
		t.Skipf("no golden vectors in %s", goldenDir)
	}

	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, strings.TrimSuffix(filepath.Base(m), ".in"))
	}

	return names
}

func readGolden(t *testing.T, name string) (input, want []byte) {
	t.Helper()

	input, err := os.ReadFile(filepath.Join(goldenDir, name+".in"))
	if err != nil {
		t.Fatalf("reading input: %v", err)
	}

	want, err = os.ReadFile(filepath.Join(goldenDir, name+".z"))
	if err != nil {
		t.Fatalf("reading expected stream: %v", err)
	}

	return input, want
}

// firstDiff reports the offset of the first differing byte, or -1 when the two
// slices share a prefix and differ only in length.
func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}

	return -1
}

func TestCompressMatchesGoldenVectors(t *testing.T) {
	for _, name := range goldenCases(t) {
		t.Run(name, func(t *testing.T) {
			input, want := readGolden(t, name)

			got := Compress(input)
			if bytes.Equal(got, want) {
				return
			}

			at := firstDiff(got, want)
			if at < 0 {
				t.Fatalf("streams share a prefix but differ in length: got %d bytes, want %d (input %d bytes)",
					len(got), len(want), len(input))
			}

			t.Fatalf("first difference at byte %d: got 0x%02x, want 0x%02x "+
				"(got %d bytes, want %d, input %d bytes)\n got: % x\nwant: % x",
				at, got[at], want[at], len(got), len(want), len(input),
				window(got, at), window(want, at))
		})
	}
}

// window returns the bytes around at, for the failure message.
func window(b []byte, at int) []byte {
	lo := max(at-8, 0)
	hi := min(at+8, len(b))

	return b[lo:hi]
}

// inflate decompresses a zlib stream through the standard library, which is an
// independent implementation of the decoder.
func inflate(t *testing.T, stream []byte) []byte {
	t.Helper()

	r, err := zlib.NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("zlib.NewReader: %v", err)
	}

	defer r.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("inflating: %v", err)
	}

	return out
}

func TestCompressRoundTrips(t *testing.T) {
	for _, name := range goldenCases(t) {
		t.Run(name, func(t *testing.T) {
			input, _ := readGolden(t, name)

			if got := inflate(t, Compress(input)); !bytes.Equal(got, input) {
				t.Errorf("round trip produced %d bytes, want %d", len(got), len(input))
			}
		})
	}
}

func TestCompressRoundTripsGenerated(t *testing.T) {
	generated := map[string][]byte{
		"nil":          nil,
		"empty":        {},
		"one":          {0x00},
		"two":          {0xff, 0x00},
		"zeros-64k":    make([]byte, 65536),
		"repeat-3":     bytes.Repeat([]byte("abc"), 40000),
		"repeat-258":   bytes.Repeat([]byte{7}, 258),
		"ascii-window": bytes.Repeat([]byte("the quick brown fox "), 5000),
		"counter":      counterBytes(1 << 17),
	}

	for name, input := range generated {
		t.Run(name, func(t *testing.T) {
			got := inflate(t, Compress(input))
			if len(input) == 0 && len(got) == 0 {
				return
			}

			if !bytes.Equal(got, input) {
				t.Errorf("round trip produced %d bytes, want %d", len(got), len(input))
			}
		})
	}
}

func counterBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * i / 7)
	}

	return b
}

func FuzzCompressRoundTrips(f *testing.F) {
	for _, name := range []string{"empty", "short-text", "runs", "schema-like"} {
		if in, err := os.ReadFile(filepath.Join(goldenDir, name+".in")); err == nil {
			f.Add(in)
		}
	}

	f.Add([]byte(nil))
	f.Add(bytes.Repeat([]byte("ab"), 300))

	f.Fuzz(func(t *testing.T, input []byte) {
		got := inflate(t, Compress(input))
		if len(got) != len(input) {
			t.Fatalf("round trip produced %d bytes, want %d", len(got), len(input))
		}

		if !bytes.Equal(got, input) {
			t.Fatal("round trip changed the data")
		}
	})
}
