package zlib1

// Tree geometry constants, all from zlib's deflate.h / trees.h.
const (
	maxBits   = 15  // MAX_BITS: maximum bit length of any Huffman code
	maxBlBits = 7   // MAX_BL_BITS: maximum bit length of the bit-length codes
	literals  = 256 // LITERALS: number of literal bytes 0..255

	lengthCodes = 29                         // LENGTH_CODES
	lCodes      = literals + 1 + lengthCodes // L_CODES: literals + END_BLOCK + length codes
	dCodes      = 30                         // D_CODES: distance codes
	blCodes     = 19                         // BL_CODES: bit-length codes

	heapSize = 2*lCodes + 1 // HEAP_SIZE: maximum heap size

	endBlock  = 256 // END_BLOCK: the end-of-block literal/length code
	rep36     = 16  // REP_3_6: repeat the previous length 3..6 times
	repz310   = 17  // REPZ_3_10: repeat a zero length 3..10 times
	repz11138 = 18  // REPZ_11_138: repeat a zero length 11..138 times

	storedBlock = 0 // STORED_BLOCK
	staticTrees = 1 // STATIC_TREES
	dynTrees    = 2 // DYN_TREES
)

// extraLbits is zlib's extra_lbits: extra bits carried by each length code.
var extraLbits = [lengthCodes]int{
	0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2,
	3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0,
}

// extraDbits is zlib's extra_dbits: extra bits carried by each distance code.
var extraDbits = [dCodes]int{
	0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6,
	7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13,
}

// extraBlbits is zlib's extra_blbits: extra bits carried by each bit-length
// code.
var extraBlbits = [blCodes]int{
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 3, 7,
}

// blOrder is zlib's bl_order: the order in which the bit-length code lengths
// are transmitted, chosen so trailing entries are the ones most likely to be
// zero and therefore trimmable.
var blOrder = [blCodes]int{
	16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15,
}

// ctData mirrors zlib's ct_data. In C, freq and code share a union, as do dad
// and length; nothing in zlib reads a field after the aliasing field has
// overwritten it, so keeping them separate here is equivalent (see build_tree,
// gen_bitlen and gen_codes, which are the only writers).
type ctData struct {
	freq   uint16 // Freq: frequency count
	code   uint16 // Code: the bit string, already bit-reversed
	dad    uint16 // Dad: father node in the Huffman tree
	length uint16 // Len: length of the bit string
}

// The static tables below are what zlib's tr_static_init computes when zlib is
// built without the pre-generated trees.h (the GEN_TREES_H path). Computing
// them the same way keeps the derivation visible instead of pasting 288 magic
// numbers.
var (
	baseLength = buildBaseLength()
	lengthCode = buildLengthCode()
	baseDist   = buildBaseDist()
	distCode   = buildDistCode()

	staticLtree = buildStaticLtree()
	staticDtree = buildStaticDtree()
)

// buildBaseLength computes zlib's base_length: the first match length (minus
// minMatch) covered by each length code.
func buildBaseLength() [lengthCodes]int {
	var (
		base   [lengthCodes]int
		length int
	)

	for code := range lengthCodes - 1 {
		base[code] = length
		length += 1 << extraLbits[code]
	}

	return base
}

// buildLengthCode computes zlib's _length_code: match length (minus minMatch)
// to length code, for lengths 0..255.
func buildLengthCode() [maxMatch - minMatch + 1]uint8 {
	var (
		codes  [maxMatch - minMatch + 1]uint8
		length int
	)

	code := 0
	for code = range lengthCodes - 1 {
		for range 1 << extraLbits[code] {
			codes[length] = uint8(code)
			length++
		}
	}

	// Match length 258 can be coded either as code 284 plus five extra bits or
	// as code 285 with none; zlib overwrites the last entry to pick the latter.
	codes[length-1] = uint8(code + 1) //nolint:gosec // code+1 <= lengthCodes-1 = 28.

	return codes
}

// buildBaseDist computes zlib's base_dist: the first distance covered by each
// distance code.
func buildBaseDist() [dCodes]int {
	var (
		base [dCodes]int
		dist int
	)

	for code := range 16 {
		base[code] = dist
		dist += 1 << extraDbits[code]
	}

	// From code 16 on, zlib works in units of 128 distances.
	dist >>= 7

	for code := 16; code < dCodes; code++ {
		base[code] = dist << 7
		dist += 1 << (extraDbits[code] - 7)
	}

	return base
}

// buildDistCode computes zlib's _dist_code. The first 256 entries map a
// distance 0..255 directly; the remaining 256 map (distance >> 7).
func buildDistCode() [512]uint8 {
	var (
		codes [512]uint8
		dist  int
	)

	for code := range 16 {
		for range 1 << extraDbits[code] {
			codes[dist] = uint8(code)
			dist++
		}
	}

	dist >>= 7

	for code := 16; code < dCodes; code++ {
		for range 1 << (extraDbits[code] - 7) {
			codes[256+dist] = uint8(code)
			dist++
		}
	}

	return codes
}

// dCode is zlib's d_code macro: map a distance (already decremented by one) to
// its distance code.
func dCode(dist int) int {
	if dist < 256 {
		return int(distCode[dist])
	}

	return int(distCode[256+(dist>>7)])
}

// buildStaticLtree computes zlib's static_ltree: the fixed literal/length tree
// of RFC 1951 section 3.2.6.
func buildStaticLtree() [lCodes + 2]ctData {
	var (
		tree    [lCodes + 2]ctData
		blCount [maxBits + 1]uint16
	)

	setLen := func(from, to int, bits uint16) {
		for n := from; n <= to; n++ {
			tree[n].length = bits
			blCount[bits]++
		}
	}

	setLen(0, 143, 8)
	setLen(144, 255, 9)
	setLen(256, 279, 7)
	setLen(280, 287, 8)

	genCodes(tree[:], lCodes+1, &blCount)

	return tree
}

// buildStaticDtree computes zlib's static_dtree: 30 distance codes of five bits
// each, stored bit-reversed.
func buildStaticDtree() [dCodes]ctData {
	var tree [dCodes]ctData

	for n := range dCodes {
		tree[n].length = 5
		tree[n].code = uint16(biReverse(uint32(n), 5)) //nolint:gosec // a 5-bit reversal fits in uint16.
	}

	return tree
}
