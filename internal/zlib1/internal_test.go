package zlib1

import (
	"bytes"
	"testing"
)

func TestBitWriterPacksLSBFirst(t *testing.T) {
	tests := []struct {
		name string
		emit func(w *bitWriter)
		want []byte
	}{
		{
			name: "eight single bits fill one byte",
			emit: func(w *bitWriter) {
				for range 8 {
					w.sendBits(1, 1)
				}
			},
			want: []byte{0xff},
		},
		{
			// The three-bit block header of a final static block, followed by
			// the seven-bit end-of-block code 0000000: exactly what Compress
			// emits for an empty input.
			name: "final static block header and end of block",
			emit: func(w *bitWriter) {
				w.sendBits(staticTrees<<1+1, 3)
				w.sendBits(0, 7)
			},
			want: []byte{0x03, 0x00},
		},
		{
			// Crossing the 16-bit accumulator: the low half spills as a short,
			// the leftover bit is wound up into a third byte.
			name: "spill across the accumulator",
			emit: func(w *bitWriter) {
				w.sendBits(0x1234, 16)
				w.sendBits(1, 1)
			},
			want: []byte{0x34, 0x12, 0x01},
		},
		{
			name: "windup pads the last byte with zeros",
			emit: func(w *bitWriter) {
				w.sendBits(0b101, 3)
			},
			want: []byte{0b101},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w bitWriter

			tt.emit(&w)
			w.biWindup()

			if !bytes.Equal(w.out, tt.want) {
				t.Errorf("out = % x, want % x", w.out, tt.want)
			}

			if w.biValid != 0 || w.biBuf != 0 {
				t.Errorf("after biWindup: biBuf = %#x, biValid = %d, want both zero", w.biBuf, w.biValid)
			}
		})
	}
}

func TestBitFlushEmitsWholeBytesOnly(t *testing.T) {
	var w bitWriter

	w.sendBits(0b1011, 4)
	w.biFlush()

	if len(w.out) != 0 {
		t.Errorf("biFlush with 4 bits pending wrote % x, want nothing", w.out)
	}

	w.sendBits(0b1111, 4)
	w.biFlush()

	if want := []byte{0b11111011}; !bytes.Equal(w.out, want) {
		t.Errorf("out = % x, want % x", w.out, want)
	}

	if w.biValid != 0 {
		t.Errorf("biValid = %d, want 0", w.biValid)
	}
}

func TestBiReverse(t *testing.T) {
	tests := []struct {
		code   uint32
		length int
		want   uint32
	}{
		{0b0, 1, 0b0},
		{0b1, 1, 0b1},
		{0b10, 2, 0b01},
		{0b110, 3, 0b011},
		{0b111, 3, 0b111},
		{0b10000000, 8, 0b00000001},
	}

	for _, tt := range tests {
		if got := biReverse(tt.code, tt.length); got != tt.want {
			t.Errorf("biReverse(%b, %d) = %b, want %b", tt.code, tt.length, got, tt.want)
		}
	}
}

// TestBuildTreeSmallFrequencyTable pins build_tree against a table small enough
// to work out by hand. Four distance codes with frequencies 4, 2, 1, 1 give the
// Huffman lengths 1, 2, 3, 3; the canonical codes for those lengths are 0, 10,
// 110 and 111, and every code is stored bit-reversed because deflate writes
// them LSB-first.
func TestBuildTreeSmallFrequencyTable(t *testing.T) {
	s := newDeflateState(nil)

	freqs := [4]uint16{0: 1, 1: 1, 2: 2, 3: 4}
	for n, f := range freqs {
		s.dynDtree[n].freq = f
	}

	s.buildTree(&s.dDesc)

	want := []struct {
		length uint16
		code   uint16
	}{
		0: {3, 0b011}, // canonical 110, reversed
		1: {3, 0b111}, // canonical 111, reversed
		2: {2, 0b01},  // canonical 10, reversed
		3: {1, 0b0},   // canonical 0
	}

	if s.dDesc.maxCode != 3 {
		t.Fatalf("maxCode = %d, want 3", s.dDesc.maxCode)
	}

	for n, w := range want {
		got := s.dynDtree[n]
		if got.length != w.length || got.code != w.code {
			t.Errorf("code %d: length = %d code = %0*b, want length %d code %0*b",
				n, got.length, w.length, got.code, w.length, w.length, w.code)
		}
	}

	// opt_len is the total code length in bits: 4*1 + 2*2 + 1*3 + 1*3. The
	// distance codes 0..3 carry no extra bits, so nothing else contributes.
	if want := uint64(4*1 + 2*2 + 1*3 + 1*3); s.optLen != want {
		t.Errorf("optLen = %d, want %d", s.optLen, want)
	}
}

// TestBuildTreeForcesTwoCodes covers build_tree's degenerate case: the deflate
// format needs two distinct codes even when the block uses one symbol or none,
// so zlib invents them.
func TestBuildTreeForcesTwoCodes(t *testing.T) {
	s := newDeflateState(nil)

	s.buildTree(&s.dDesc)

	if s.dDesc.maxCode != 1 {
		t.Fatalf("maxCode = %d, want 1 (two invented codes)", s.dDesc.maxCode)
	}

	for n := range 2 {
		if got := s.dynDtree[n].length; got != 1 {
			t.Errorf("code %d: length = %d, want 1", n, got)
		}
	}
}
