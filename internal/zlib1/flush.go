package zlib1

// initBlock is zlib's init_block: clear the frequency counts for a new block.
// END_BLOCK is pre-charged with one occurrence because every block ends with
// it.
func (s *deflateState) initBlock() {
	for n := range lCodes {
		s.dynLtree[n].freq = 0
	}

	for n := range dCodes {
		s.dynDtree[n].freq = 0
	}

	for n := range blCodes {
		s.blTree[n].freq = 0
	}

	s.dynLtree[endBlock].freq = 1
	s.optLen = 0
	s.staticLen = 0
	s.symNext = 0
	s.matches = 0
}

// tallyLit is zlib's _tr_tally_lit. It returns true when the symbol buffer is
// full and the block must be flushed.
func (s *deflateState) tallyLit(c byte) bool {
	s.symBuf[s.symNext] = 0
	s.symBuf[s.symNext+1] = 0
	s.symBuf[s.symNext+2] = c
	s.symNext += 3

	s.dynLtree[c].freq++

	return s.symNext == symEnd
}

// tallyDist is zlib's _tr_tally_dist. dist is the match distance and lc the
// match length minus minMatch.
func (s *deflateState) tallyDist(dist, lc int) bool {
	// The symbol buffer is zlib's sym_buf: three bytes per symbol, so both the
	// distance (< 32768) and the length code (<= 255) are stored truncated.
	s.symBuf[s.symNext] = byte(dist)        //nolint:gosec // deliberate: the low byte of the distance.
	s.symBuf[s.symNext+1] = byte(dist >> 8) //nolint:gosec // deliberate: the high byte of the distance.
	s.symBuf[s.symNext+2] = byte(lc)        //nolint:gosec // lc = match length - minMatch <= 255.
	s.symNext += 3

	s.matches++

	dist--
	s.dynLtree[int(lengthCode[lc])+literals+1].freq++
	s.dynDtree[dCode(dist)].freq++

	return s.symNext == symEnd
}

// sendCode is zlib's send_code macro.
func (s *deflateState) sendCode(c int, tree []ctData) {
	s.sendBits(uint32(tree[c].code), int(tree[c].length))
}

// scanState carries the running state shared by scan_tree and send_tree, whose
// loops are identical apart from what they do at each run boundary.
type scanState struct {
	prevlen  int
	curlen   int
	nextlen  int
	count    int
	maxCount int
	minCount int
}

// newScanState reproduces the prologue that scan_tree and send_tree share.
func newScanState(tree []ctData) *scanState {
	st := &scanState{prevlen: -1, nextlen: int(tree[0].length), maxCount: 7, minCount: 4}
	if st.nextlen == 0 {
		st.maxCount, st.minCount = 138, 3
	}

	return st
}

// advance moves to the next run and reports whether the current run ended. It
// is the shared body of scan_tree's and send_tree's loop.
func (st *scanState) advance(tree []ctData, n int) bool {
	st.curlen = st.nextlen
	st.nextlen = int(tree[n+1].length)
	st.count++

	return st.count >= st.maxCount || st.curlen != st.nextlen
}

// endRun reproduces the run-boundary bookkeeping shared by both functions.
func (st *scanState) endRun() {
	st.count = 0
	st.prevlen = st.curlen

	switch {
	case st.nextlen == 0:
		st.maxCount, st.minCount = 138, 3
	case st.curlen == st.nextlen:
		st.maxCount, st.minCount = 6, 3
	default:
		st.maxCount, st.minCount = 7, 4
	}
}

// scanTree is zlib's scan_tree: count the frequencies of the bit-length codes
// needed to transmit tree's code lengths.
func (s *deflateState) scanTree(tree []ctData, maxCode int) {
	st := newScanState(tree)
	tree[maxCode+1].length = 0xffff // guard: no run can extend past maxCode

	for n := 0; n <= maxCode; n++ {
		if !st.advance(tree, n) {
			continue
		}

		switch {
		case st.count < st.minCount:
			s.blTree[st.curlen].freq += uint16(st.count) //nolint:gosec // count <= 138.
		case st.curlen != 0:
			if st.curlen != st.prevlen {
				s.blTree[st.curlen].freq++
			}

			s.blTree[rep36].freq++
		case st.count <= 10:
			s.blTree[repz310].freq++
		default:
			s.blTree[repz11138].freq++
		}

		st.endRun()
	}
}

// sendTree is zlib's send_tree: emit tree's code lengths using the bit-length
// tree. Its structure must stay identical to scanTree's.
func (s *deflateState) sendTree(tree []ctData, maxCode int) {
	st := newScanState(tree)

	for n := 0; n <= maxCode; n++ {
		if !st.advance(tree, n) {
			continue
		}

		switch {
		case st.count < st.minCount:
			for range st.count {
				s.sendCode(st.curlen, s.blTree[:])
			}
		case st.curlen != 0:
			if st.curlen != st.prevlen {
				s.sendCode(st.curlen, s.blTree[:])
				st.count--
			}

			s.sendCode(rep36, s.blTree[:])
			s.sendBits(uint32(st.count-3), 2) //nolint:gosec // 3 <= count <= 6 here.
		case st.count <= 10:
			s.sendCode(repz310, s.blTree[:])
			s.sendBits(uint32(st.count-3), 3) //nolint:gosec // 3 <= count <= 10 here.
		default:
			s.sendCode(repz11138, s.blTree[:])
			s.sendBits(uint32(st.count-11), 7) //nolint:gosec // 11 <= count <= 138 here.
		}

		st.endRun()
	}
}

// buildBlTree is zlib's build_bl_tree. It returns max_blindex: the index of the
// last bit-length code whose length must be transmitted.
func (s *deflateState) buildBlTree() int {
	s.scanTree(s.dynLtree[:], s.lDesc.maxCode)
	s.scanTree(s.dynDtree[:], s.dDesc.maxCode)

	s.buildTree(&s.blDesc)

	// Trim the trailing bit-length codes that carry no length, in bl_order's
	// permutation. Codes 16, 17, 18 and 0 are never trimmed.
	maxBlindex := blCodes - 1
	for ; maxBlindex >= 3; maxBlindex-- {
		if s.blTree[blOrder[maxBlindex]].length != 0 {
			break
		}
	}

	s.optLen += uint64(3*(maxBlindex+1)) + 5 + 5 + 4

	return maxBlindex
}

// sendAllTrees is zlib's send_all_trees: emit the dynamic block's header.
func (s *deflateState) sendAllTrees(lcodes, dcodes, blcodes int) {
	s.sendBits(uint32(lcodes-257), 5) //nolint:gosec // 257 <= lcodes <= 286.
	s.sendBits(uint32(dcodes-1), 5)   //nolint:gosec // 1 <= dcodes <= 30.
	s.sendBits(uint32(blcodes-4), 4)  //nolint:gosec // 4 <= blcodes <= 19.

	for rank := range blcodes {
		s.sendBits(uint32(s.blTree[blOrder[rank]].length), 3)
	}

	s.sendTree(s.dynLtree[:], lcodes-1)
	s.sendTree(s.dynDtree[:], dcodes-1)
}

// compressBlock is zlib's compress_block: replay the symbol buffer through the
// given literal/length and distance trees.
func (s *deflateState) compressBlock(ltree, dtree []ctData) {
	for sx := 0; sx < s.symNext; {
		dist := int(s.symBuf[sx])
		dist += int(s.symBuf[sx+1]) << 8
		lc := int(s.symBuf[sx+2])
		sx += 3

		if dist == 0 {
			s.sendCode(lc, ltree)

			continue
		}

		code := int(lengthCode[lc])
		s.sendCode(code+literals+1, ltree)

		if extra := extraLbits[code]; extra != 0 {
			s.sendBits(uint32(lc-baseLength[code]), extra) //nolint:gosec // lc >= base_length[code] by construction.
		}

		dist--
		code = dCode(dist)
		s.sendCode(code, dtree)

		if extra := extraDbits[code]; extra != 0 {
			s.sendBits(uint32(dist-baseDist[code]), extra) //nolint:gosec // dist >= base_dist[code] by construction.
		}
	}

	s.sendCode(endBlock, ltree)
}

// trStoredBlock is zlib's _tr_stored_block: emit the block verbatim, preceded
// by a byte-aligned LEN/NLEN pair.
func (s *deflateState) trStoredBlock(buf []byte, last int) {
	s.sendBits(uint32(storedBlock<<1)+uint32(last), 3) //nolint:gosec // last is 0 or 1.
	s.biWindup()

	length := uint16(len(buf)) //nolint:gosec // stored blocks never exceed 64 KiB; see trFlushBlock.
	s.putShort(length)
	s.putShort(^length)
	s.out = append(s.out, buf...)
}

// blockLengths chooses between the stored, static and dynamic encodings exactly
// as _tr_flush_block does. optLenb and staticLenb are in bytes.
func (s *deflateState) blockLengths() (optLenb, staticLenb uint64) {
	s.buildTree(&s.lDesc)
	s.buildTree(&s.dDesc)

	maxBlindex := s.buildBlTree()

	optLenb = (s.optLen + 3 + 7) >> 3
	staticLenb = (s.staticLen + 3 + 7) >> 3

	s.maxBlindex = maxBlindex

	if staticLenb <= optLenb {
		optLenb = staticLenb
	}

	return optLenb, staticLenb
}

// trFlushBlock is zlib's _tr_flush_block. buf is the input block (nil when
// block_start went negative and the raw bytes are no longer addressable), and
// last marks the final block of the stream.
func (s *deflateState) trFlushBlock(buf []byte, bufValid bool, storedLen int, last int) {
	optLenb, staticLenb := s.blockLengths()

	switch {
	case uint64(storedLen)+4 <= optLenb && bufValid: //nolint:gosec // storedLen is a window offset difference and never negative.
		// A stored block costs four bytes of overhead on top of the data; if
		// that beats both trees, zlib takes it. This is the path random data
		// ends up on.
		s.trStoredBlock(buf, last)
	case staticLenb == optLenb:
		s.sendBits(uint32(staticTrees<<1)+uint32(last), 3) //nolint:gosec // last is 0 or 1.
		s.compressBlock(staticLtree[:], staticDtree[:])
	default:
		s.sendBits(uint32(dynTrees<<1)+uint32(last), 3) //nolint:gosec // last is 0 or 1.
		s.sendAllTrees(s.lDesc.maxCode+1, s.dDesc.maxCode+1, s.maxBlindex+1)
		s.compressBlock(s.dynLtree[:], s.dynDtree[:])
	}

	s.initBlock()

	if last != 0 {
		s.biWindup()
	}
}

// flushBlock is zlib's FLUSH_BLOCK macro: emit everything between block_start
// and strstart, then restart the block at strstart.
func (s *deflateState) flushBlock(last int) {
	var (
		buf      []byte
		bufValid = s.blockStart >= 0
	)

	storedLen := s.strstart - s.blockStart

	if bufValid {
		buf = s.window[s.blockStart : s.blockStart+storedLen]
	}

	s.trFlushBlock(buf, bufValid, storedLen, last)
	s.blockStart = s.strstart
}
