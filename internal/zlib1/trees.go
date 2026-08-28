package zlib1

// staticTreeDesc mirrors zlib's static_tree_desc.
type staticTreeDesc struct {
	staticTree []ctData // the corresponding static tree, or nil for the bit-length tree
	extraBits  []int    // extra bits for each code, or nil
	extraBase  int      // base index for extraBits
	elems      int      // max number of elements in the tree
	maxLength  int      // max bit length for the codes
}

// treeDesc mirrors zlib's tree_desc.
type treeDesc struct {
	dynTree []ctData
	maxCode int
	stat    *staticTreeDesc
}

// The three static descriptors of zlib's trees.c.
var (
	staticLDesc = staticTreeDesc{
		staticTree: staticLtree[:],
		extraBits:  extraLbits[:],
		extraBase:  literals + 1,
		elems:      lCodes,
		maxLength:  maxBits,
	}
	staticDDesc = staticTreeDesc{
		staticTree: staticDtree[:],
		extraBits:  extraDbits[:],
		extraBase:  0,
		elems:      dCodes,
		maxLength:  maxBits,
	}
	staticBlDesc = staticTreeDesc{
		staticTree: nil,
		extraBits:  extraBlbits[:],
		extraBase:  0,
		elems:      blCodes,
		maxLength:  maxBlBits,
	}
)

// genCodes is zlib's gen_codes: turn a table of code lengths into the canonical
// Huffman codes of RFC 1951 section 3.2.2, stored bit-reversed for LSB-first
// output.
func genCodes(tree []ctData, maxCode int, blCount *[maxBits + 1]uint16) {
	var nextCode [maxBits + 1]uint16

	code := uint16(0)

	for bits := 1; bits <= maxBits; bits++ {
		code = (code + blCount[bits-1]) << 1
		nextCode[bits] = code
	}

	for n := 0; n <= maxCode; n++ {
		length := int(tree[n].length)
		if length == 0 {
			continue
		}

		tree[n].code = uint16(biReverse(uint32(nextCode[length]), length)) //nolint:gosec // a reversal of <= 15 bits fits in uint16.
		nextCode[length]++
	}
}

// smaller is zlib's smaller macro. The depth tie-break is what makes zlib's
// tree shapes reproducible: two symbols of equal frequency are ordered by the
// depth of the subtree they root, and equal depths keep the earlier heap entry.
func (s *deflateState) smaller(tree []ctData, n, m int) bool {
	return tree[n].freq < tree[m].freq ||
		(tree[n].freq == tree[m].freq && s.depth[n] <= s.depth[m])
}

// pqdownheap is zlib's pqdownheap: restore the heap property after the element
// at index k has been replaced.
func (s *deflateState) pqdownheap(tree []ctData, k int) {
	v := s.heap[k]
	j := k << 1 // left son of k

	for j <= s.heapLen {
		// Pick the smaller of the two sons.
		if j < s.heapLen && s.smaller(tree, s.heap[j+1], s.heap[j]) {
			j++
		}

		if s.smaller(tree, v, s.heap[j]) {
			break
		}

		s.heap[k] = s.heap[j]
		k = j
		j <<= 1
	}

	s.heap[k] = v
}

// initHeap is the first half of zlib's build_tree: seed the heap with every
// symbol that actually occurs, forcing at least two leaves to exist. It returns
// the largest code with a non-zero frequency.
func (s *deflateState) initHeap(desc *treeDesc) int {
	tree := desc.dynTree
	stree := desc.stat.staticTree
	maxCode := -1

	s.heapLen = 0
	s.heapMax = heapSize

	for n := range desc.stat.elems {
		if tree[n].freq != 0 {
			s.heapLen++
			s.heap[s.heapLen] = n
			maxCode = n
			s.depth[n] = 0
		} else {
			tree[n].length = 0
		}
	}

	// The pkzip format requires at least two distinct codes, even for a
	// completely empty tree. zlib invents them and pays for them by decrementing
	// opt_len/static_len, which underflows to a huge unsigned value when the
	// counters are still zero. That underflow is load-bearing: it is what makes
	// a degenerate dynamic tree lose to the static tree in _tr_flush_block.
	for s.heapLen < 2 {
		s.heapLen++

		node := 0

		if maxCode < 2 {
			maxCode++
			node = maxCode
		}

		s.heap[s.heapLen] = node
		tree[node].freq = 1
		s.depth[node] = 0
		s.optLen--

		if stree != nil {
			s.staticLen -= uint64(stree[node].length)
		}
	}

	return maxCode
}

// buildTree is zlib's build_tree: construct the optimal Huffman tree for the
// frequencies in desc.dynTree, then compute the bit lengths and codes.
func (s *deflateState) buildTree(desc *treeDesc) {
	tree := desc.dynTree

	maxCode := s.initHeap(desc)
	desc.maxCode = maxCode

	// The elements heap[heapLen/2+1 .. heapLen] are leaves of the heap already.
	for n := s.heapLen / 2; n >= 1; n-- {
		s.pqdownheap(tree, n)
	}

	// Repeatedly join the two least frequent nodes. node is the next free
	// internal node index; heap[heapMax..heapSize-1] collects the nodes in
	// reverse order of creation, which is the order gen_bitlen walks.
	node := desc.stat.elems

	for {
		// pqremove
		n := s.heap[1]
		s.heap[1] = s.heap[s.heapLen]
		s.heapLen--
		s.pqdownheap(tree, 1)

		m := s.heap[1] // m is the next least frequent node

		s.heapMax--
		s.heap[s.heapMax] = n
		s.heapMax--
		s.heap[s.heapMax] = m

		tree[node].freq = tree[n].freq + tree[m].freq

		if s.depth[n] >= s.depth[m] {
			s.depth[node] = s.depth[n] + 1
		} else {
			s.depth[node] = s.depth[m] + 1
		}

		//nolint:gosec // node < heapSize = 573, well inside uint16.
		tree[n].dad, tree[m].dad = uint16(node), uint16(node)

		s.heap[1] = node
		node++

		s.pqdownheap(tree, 1)

		if s.heapLen < 2 {
			break
		}
	}

	s.heapMax--
	s.heap[s.heapMax] = s.heap[1]

	s.genBitlen(desc)
	genCodes(tree, maxCode, &s.blCount)
}

// genBitlen is zlib's gen_bitlen: derive the code lengths from the tree shape,
// accumulate opt_len and static_len, and repair any length that exceeded
// maxLength.
func (s *deflateState) genBitlen(desc *treeDesc) {
	tree := desc.dynTree
	stat := desc.stat
	maxCode := desc.maxCode

	for bits := range s.blCount {
		s.blCount[bits] = 0
	}

	// The root of the heap has length 0; every other node is one bit deeper
	// than its father, which gen_bitlen has already visited.
	tree[s.heap[s.heapMax]].length = 0

	overflow := s.assignBitlens(tree, stat, maxCode)
	if overflow == 0 {
		return
	}

	s.repairOverflow(tree, stat.maxLength, maxCode, overflow)
}

// assignBitlens is the main loop of gen_bitlen. It returns the number of codes
// that had to be clamped to maxLength.
func (s *deflateState) assignBitlens(tree []ctData, stat *staticTreeDesc, maxCode int) int {
	overflow := 0

	for h := s.heapMax + 1; h < heapSize; h++ {
		n := s.heap[h]

		bits := int(tree[tree[n].dad].length) + 1
		if bits > stat.maxLength {
			bits = stat.maxLength
			overflow++
		}

		tree[n].length = uint16(bits) //nolint:gosec // bits <= maxBits = 15.

		if n > maxCode {
			continue // not a leaf node
		}

		s.blCount[bits]++

		xbits := 0
		if n >= stat.extraBase {
			xbits = stat.extraBits[n-stat.extraBase]
		}

		freq := uint64(tree[n].freq)
		s.optLen += freq * uint64(bits+xbits)

		if stat.staticTree != nil {
			//nolint:gosec // a code length plus its extra bits is a small positive count.
			s.staticLen += freq * uint64(int(stat.staticTree[n].length)+xbits)
		}
	}

	return overflow
}

// repairOverflow is the second half of gen_bitlen: move codes out of the
// overfull maxLength bucket, then rewrite the lengths of the affected symbols.
func (s *deflateState) repairOverflow(tree []ctData, maxLength, maxCode, overflow int) {
	// Find the deepest bucket that still has room and split one of its codes.
	for overflow > 0 {
		bits := maxLength - 1
		for s.blCount[bits] == 0 {
			bits--
		}

		s.blCount[bits]--      // move one leaf down the tree
		s.blCount[bits+1] += 2 // move one overflow item as its brother
		s.blCount[maxLength]-- // the brother of the moved leaf
		overflow -= 2
	}

	// Now recompute all bit lengths, scanning in increasing frequency. The
	// symbols are already sorted by frequency in heap[heapMax..heapSize-1].
	h := heapSize

	for bits := maxLength; bits != 0; bits-- {
		n := int(s.blCount[bits])

		for n != 0 {
			h--
			m := s.heap[h]

			if m > maxCode {
				continue
			}

			if int(tree[m].length) != bits {
				// zlib computes this in ulg, so the subtraction wraps when the
				// code got shorter; the wrapped value then cancels out again.
				//nolint:gosec // bits is a positive code length; the wrap is zlib's.
				s.optLen += (uint64(bits) - uint64(tree[m].length)) * uint64(tree[m].freq)
				tree[m].length = uint16(bits) //nolint:gosec // bits <= maxBits = 15.
			}

			n--
		}
	}
}
