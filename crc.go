package absdb

// absCRC32 is the CRC-32 variant used by Absolute Database for the encryption
// ControlBlock and the ABSP page checksum.
//
// It uses the reflected IEEE polynomial 0xEDB88320 with an initial register of
// 0 and no final inversion. This is deliberately NOT hash/crc32's IEEE variant,
// which pre-inverts the register to 0xFFFFFFFF and inverts the result again;
// feeding the same bytes through crc32.ChecksumIEEE yields a different value.
const absCRCPoly = 0xEDB88320

// absCRCTable is the byte-wise lookup table for absCRCPoly.
var absCRCTable = makeAbsCRCTable()

func makeAbsCRCTable() [256]uint32 {
	var table [256]uint32

	for i := range table {
		crc := uint32(i)

		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ absCRCPoly
			} else {
				crc >>= 1
			}
		}

		table[i] = crc
	}

	return table
}

// absCRC32 computes the Absolute Database CRC-32 of data.
func absCRC32(data []byte) uint32 {
	crc := uint32(0)

	for _, b := range data {
		crc = crc>>8 ^ absCRCTable[byte(crc)^b]
	}

	return crc
}
