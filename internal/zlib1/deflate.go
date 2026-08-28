package zlib1

// Window and hash geometry, all from zlib's deflate.h with windowBits = 15 and
// memLevel = 8 (the deflateInit defaults, which is what the Absolute Database
// engine uses).
const (
	minMatch = 3   // MIN_MATCH
	maxMatch = 258 // MAX_MATCH
	winInit  = maxMatch

	wBits = 15
	wSize = 1 << wBits // 32 KiB window
	wMask = wSize - 1

	windowSize = 2 * wSize

	// MIN_LOOKAHEAD: the minimum lookahead longest_match needs, and MAX_DIST:
	// the furthest back a match may reach so that longest_match never runs off
	// the end of the window.
	minLookahead = maxMatch + minMatch + 1
	maxDist      = wSize - minLookahead

	hashBits  = 15 // memLevel + 7
	hashSize  = 1 << hashBits
	hashMask  = hashSize - 1
	hashShift = 5 // (hashBits + minMatch - 1) / minMatch

	litBufsize = 1 << 14              // 1 << (memLevel + 6)
	symEnd     = (litBufsize - 1) * 3 // sym_end: the symbol buffer's high-water mark
)

// zlib's configuration_table row for level 1:
//
//	/*      good_length  max_lazy  nice_length  max_chain  func          */
//	/* 1 */ {4,          4,        8,           4,         deflate_fast}
const (
	goodMatch      = 4 // good_length: shorten the hash chain once a long match is in hand
	maxInsertLen   = 4 // max_lazy, used by deflate_fast as max_insert_length
	niceMatchLevel = 8 // nice_length: stop searching once a match this long is found
	maxChainLength = 4 // max_chain
)

// deflateState mirrors the parts of zlib's deflate_state that level 1 uses.
type deflateState struct {
	bitWriter

	src    []byte // the whole input, standing in for strm->next_in
	srcPos int    // consumed prefix of src, standing in for strm->avail_in

	window []byte   // 2*wSize sliding window; the lower half is history
	prev   []uint16 // prev[pos & wMask]: previous position with the same hash
	head   []uint16 // head[h]: most recent position with hash h

	// highWater is zlib's high_water: how far into the window bytes have been
	// written or deliberately zeroed. A window slide deliberately does *not*
	// rewind it, which is why stale bytes survive above the data; see
	// zeroHighWater.
	highWater int

	insH       uint32 // ins_h: the rolling hash of the next three bytes
	strstart   int    // strstart: current position in the window
	lookahead  int    // lookahead: valid bytes ahead of strstart
	insert     int    // insert: bytes not yet hashed after a fill
	blockStart int    // block_start: window offset the current block starts at
	matchStart int    // match_start: start of the match found by longest_match
	matchLen   int    // match_length
	prevLength int    // prev_length; deflate_fast leaves it at minMatch-1

	symBuf  []byte // sym_buf: three bytes per symbol (distance low, high, length/literal)
	symNext int    // sym_next: write offset into symBuf
	matches int    // matches: number of matches in the current block

	dynLtree [heapSize]ctData
	dynDtree [2*dCodes + 1]ctData
	blTree   [2*blCodes + 1]ctData

	lDesc, dDesc, blDesc treeDesc

	blCount [maxBits + 1]uint16
	heap    [heapSize]int
	depth   [heapSize]uint8
	heapLen int
	heapMax int

	optLen     uint64 // opt_len: bit length of the current block with the dynamic trees
	staticLen  uint64 // static_len: bit length of the current block with the static trees
	maxBlindex int    // build_bl_tree's return value, kept for send_all_trees
}

// newDeflateState is zlib's deflateInit2_ plus lm_init, for level 1.
func newDeflateState(src []byte) *deflateState {
	s := &deflateState{
		src: src,
		// The window is one byte-scan longer than zlib's so that a bounds check
		// can never fail on the trailing comparison in longestMatch; zlib
		// itself never reads past windowSize, so the extra bytes stay zero and
		// cannot influence the output.
		window: make([]byte, windowSize+maxMatch),
		prev:   make([]uint16, wSize),
		head:   make([]uint16, hashSize),
		symBuf: make([]byte, litBufsize*3),
		// out is sized for the worst case zlib itself allows: stored blocks
		// grow the data by five bytes per 64 KiB, plus the zlib wrapper.
		bitWriter:  bitWriter{out: make([]byte, 0, len(src)+len(src)/1000+64)},
		matchLen:   minMatch - 1,
		prevLength: minMatch - 1,
	}

	s.lDesc = treeDesc{dynTree: s.dynLtree[:], stat: &staticLDesc}
	s.dDesc = treeDesc{dynTree: s.dynDtree[:], stat: &staticDDesc}
	s.blDesc = treeDesc{dynTree: s.blTree[:], stat: &staticBlDesc}

	s.initBlock()

	return s
}

// updateHash is zlib's UPDATE_HASH macro. The hash folds in the byte
// minMatch-1 positions ahead, so after three updates it identifies a
// three-byte string.
func updateHash(h uint32, c byte) uint32 {
	return ((h << hashShift) ^ uint32(c)) & hashMask
}

// insertString is zlib's INSERT_STRING macro: hash the three bytes at str and
// push str onto the front of that hash's chain. It returns the previous head of
// the chain, which is the first candidate longest_match will look at.
func (s *deflateState) insertString(str int) int {
	s.insH = updateHash(s.insH, s.window[str+minMatch-1])

	head := s.head[s.insH]
	s.prev[str&wMask] = head
	s.head[s.insH] = uint16(str) //nolint:gosec // str < 2*wSize and zlib's Pos is a 16-bit type too; the truncation is the algorithm.

	return int(head)
}

// slideHash is zlib's slide_hash: rebase every chain entry by one window after
// the window has been slid down.
func (s *deflateState) slideHash() {
	for n := range s.head {
		if m := s.head[n]; m >= wSize {
			s.head[n] = m - wSize
		} else {
			s.head[n] = 0 // NIL
		}
	}

	for n := range s.prev {
		if m := s.prev[n]; m >= wSize {
			s.prev[n] = m - wSize
		} else {
			s.prev[n] = 0 // NIL
		}
	}
}

// readBuf is zlib's read_buf: move up to size bytes of input into the window.
func (s *deflateState) readBuf(start, size int) int {
	n := min(len(s.src)-s.srcPos, size)

	copy(s.window[start:start+n], s.src[s.srcPos:s.srcPos+n])
	s.srcPos += n

	return n
}

// slideWindow is the window-sliding half of fill_window. It returns the extra
// free space the slide created.
func (s *deflateState) slideWindow(more int) int {
	copy(s.window[:wSize-more], s.window[wSize:wSize+wSize-more])

	s.matchStart -= wSize
	s.strstart -= wSize
	s.blockStart -= wSize

	if s.insert > s.strstart {
		s.insert = s.strstart
	}

	s.slideHash()

	return more + wSize
}

// hashPending is the "initialize the hash value now that we have some input"
// tail of fill_window: hash and chain every position that was read but not yet
// inserted.
func (s *deflateState) hashPending() {
	if s.lookahead+s.insert < minMatch {
		return
	}

	str := s.strstart - s.insert
	s.insH = uint32(s.window[str])
	s.insH = updateHash(s.insH, s.window[str+1])

	for s.insert != 0 {
		s.insH = updateHash(s.insH, s.window[str+minMatch-1])
		s.prev[str&wMask] = s.head[s.insH]
		s.head[s.insH] = uint16(str) //nolint:gosec // str < 2*wSize; see insertString.
		str++
		s.insert--

		if s.lookahead+s.insert < minMatch {
			break
		}
	}
}

// zeroHighWater is the tail of fill_window that zeroes the bytes past the data
// so longest_match never reads uninitialised memory. It is reproduced because
// high_water is deliberately *not* rewound by a window slide, which leaves
// stale bytes above the data that longest_match can and does compare against.
func (s *deflateState) zeroHighWater() {
	if s.highWater >= windowSize {
		return
	}

	curr := s.strstart + s.lookahead

	switch {
	case s.highWater < curr:
		init := min(windowSize-curr, winInit)

		clear(s.window[curr : curr+init])

		s.highWater = curr + init
	case s.highWater < curr+winInit:
		init := min(curr+winInit-s.highWater, windowSize-s.highWater)

		clear(s.window[s.highWater : s.highWater+init])

		s.highWater += init
	}
}

// fillWindow is zlib's fill_window.
func (s *deflateState) fillWindow() {
	for {
		more := windowSize - s.lookahead - s.strstart

		if s.strstart >= wSize+maxDist {
			more = s.slideWindow(more)
		}

		if s.srcPos == len(s.src) {
			break
		}

		s.lookahead += s.readBuf(s.strstart+s.lookahead, more)
		s.hashPending()

		if s.lookahead >= minLookahead || s.srcPos == len(s.src) {
			break
		}
	}

	s.zeroHighWater()
}

// longestMatch is zlib's longest_match (the portable, non-UNALIGNED_OK
// variant). curMatch is the head of the hash chain to walk; the return value is
// the length of the best match found, with matchStart set to its position.
//
// The result may exceed the lookahead, in which case it is truncated here, just
// as zlib does.
func (s *deflateState) longestMatch(curMatch int) int {
	chainLength := maxChainLength
	bestLen := s.prevLength
	niceMatch := niceMatchLevel

	// zlib shortens the chain once a good match is already in hand. In
	// deflate_fast prev_length never leaves minMatch-1, so this never fires;
	// it is reproduced so the function stays a faithful copy.
	if s.prevLength >= goodMatch {
		chainLength >>= 2
	}

	if niceMatch > s.lookahead {
		niceMatch = s.lookahead
	}

	limit := 0 // NIL
	if s.strstart > maxDist {
		limit = s.strstart - maxDist
	}

	scan := s.strstart
	scanEnd1 := s.window[scan+bestLen-1]
	scanEnd := s.window[scan+bestLen]

	for {
		if n := s.matchRun(scan, curMatch, bestLen, scanEnd1, scanEnd); n > bestLen {
			s.matchStart = curMatch
			bestLen = n

			if n >= niceMatch {
				break
			}

			scanEnd1 = s.window[scan+bestLen-1]
			scanEnd = s.window[scan+bestLen]
		}

		curMatch = int(s.prev[curMatch&wMask])
		chainLength--

		if curMatch <= limit || chainLength == 0 {
			break
		}
	}

	if bestLen <= s.lookahead {
		return bestLen
	}

	return s.lookahead
}

// matchRun is the body of longest_match's chain loop: reject the candidate at
// match with zlib's four cheap tests, then measure how far it agrees with the
// string at scan. It returns 0 for a rejected candidate.
func (s *deflateState) matchRun(scan, match, bestLen int, scanEnd1, scanEnd byte) int {
	if s.window[match+bestLen] != scanEnd ||
		s.window[match+bestLen-1] != scanEnd1 ||
		s.window[match] != s.window[scan] ||
		s.window[match+1] != s.window[scan+1] {
		return 0
	}

	// Bytes 0 and 1 matched, and byte 2 is implied by the scanEnd checks, so
	// zlib resumes the comparison at offset 3.
	n := 3
	for n < maxMatch && s.window[scan+n] == s.window[match+n] {
		n++
	}

	return n
}

// insertMatchInterior is the max_insert_length branch of deflate_fast. zlib
// only hashes the interior positions of a match when the match is short enough;
// for a longer one it skips them and merely restarts the rolling hash past the
// match. Which of the two runs changes the hash chains, and so changes the
// matches found later and the bytes emitted.
func (s *deflateState) insertMatchInterior() {
	if s.matchLen <= maxInsertLen && s.lookahead >= minMatch {
		s.matchLen-- // string at strstart already in table

		for {
			s.strstart++
			s.insertString(s.strstart)

			s.matchLen--
			if s.matchLen == 0 {
				break
			}
		}

		s.strstart++

		return
	}

	s.strstart += s.matchLen
	s.matchLen = 0
	s.insH = uint32(s.window[s.strstart])
	s.insH = updateHash(s.insH, s.window[s.strstart+1])
	// Not valid for minMatch != 3, exactly as zlib notes.
}

// deflateFast is zlib's deflate_fast, specialised to a single call with the
// whole input available and Z_FINISH.
func (s *deflateState) deflateFast() {
	for {
		// Make sure there is always enough lookahead for longestMatch; at the
		// end of the input the lookahead simply runs out.
		if s.lookahead < minLookahead {
			s.fillWindow()

			if s.lookahead == 0 {
				break
			}
		}

		hashHead := 0
		if s.lookahead >= minMatch {
			hashHead = s.insertString(s.strstart)
		}

		if hashHead != 0 && s.strstart-hashHead <= maxDist {
			s.matchLen = s.longestMatch(hashHead)
		}

		var bflush bool

		if s.matchLen >= minMatch {
			bflush = s.tallyDist(s.strstart-s.matchStart, s.matchLen-minMatch)
			s.lookahead -= s.matchLen

			s.insertMatchInterior()
		} else {
			bflush = s.tallyLit(s.window[s.strstart])
			s.lookahead--
			s.strstart++
		}

		if bflush {
			s.flushBlock(0)
		}
	}

	s.insert = min(s.strstart, minMatch-1)
	s.flushBlock(1)
}
