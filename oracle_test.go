package absdb

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// The oracle: the record decoder and the B-tree leaf scan are two independent
// answers to the same question — which rows does this table hold, and where
// does each one live?
//
// The record decoder derives a fixed record layout from the schema and walks
// the occupancy bitmap of every type-10 page. The leaf scan walks the type-12
// index pages and reads a (PageNo, ItemNo) reference out of every leaf entry.
// Neither reads a byte the other depends on. When the two disagree, one of
// them is wrong — and for months the record decoder was, silently, because
// nothing ever compared them.
//
// These tests compare them, for every fixture and every user index.

// fixturePassword is the password of the encrypted Addresses fixtures.
const fixturePassword = "Bla"

// recordID identifies one row by the data page it lives on and its slot within
// that page.
type recordID struct {
	PageNo int32
	ItemNo uint16
}

func (id recordID) String() string {
	return fmt.Sprintf("(page %d, item %d)", id.PageNo, id.ItemNo)
}

// oracleRowCounts pins the absolute row count of every fixture, measured
// directly from the file bytes (GROUND_TRUTH §3). The cross-check below is the
// primary assertion; these numbers additionally catch a regression that moved
// the reader and the leaf scan together.
var oracleRowCounts = map[string]int{
	"TS03.abs":                   18,
	"RREC0011.abs":               30,
	"RCON0011.abs":               300,
	"RCFQ0011.abs":               600,
	"RMPA0011.abs":               600,
	"RR240011.abs":               30,
	"RPDG0011.abs":               30,
	"RFRQ0011.abs":               60,
	"RGRP0011.abs":               30,
	"RMND0011.abs":               10,
	"RRAI0011.abs":               5,
	"RRAD0011.abs":               20,
	"Addresses.abs":              0,
	"Addresses-Blowfish.abs":     0,
	"Addresses-DES_Single.abs":   0,
	"Addresses-Rijndael_128.abs": 0,
	"Writes.abs":                 3,
	"Writes-ins1.abs":            4,
	"Writes-ins2.abs":            5,
	"Writes-upd.abs":             3,
	"Writes-updname.abs":         3,
	"Writes-idx.abs":             3,
	"Writes-idx-ins.abs":         4,
	"Writes-upd2.abs":            3,
	"Writes-del2.abs":            1,
	"Writes-del.abs":             2,
	"Writes-delins.abs":          3,
}

// unindexedFixtures are the fixtures deliberately created without a user
// index, because this package refuses to insert into or delete from an indexed
// table (see ErrIndexNotMaintained) and the write tests need a table it will
// accept. They are named here rather than detected, so that an index silently
// disappearing from any other fixture still fails the cross-check below.
var unindexedFixtures = map[string]bool{
	"Writes.abs":         true,
	"Writes-ins1.abs":    true,
	"Writes-ins2.abs":    true,
	"Writes-upd.abs":     true,
	"Writes-updname.abs": true,
	"Writes-upd2.abs":    true,
	"Writes-del2.abs":    true,
	"Writes-del.abs":     true,
	"Writes-delins.abs":  true,
}

// fixtureNames returns every testdata/*.abs in sorted order. It skips the test
// when the directory is absent: testdata holds real customer data and is not
// committed, so a fresh clone has to skip rather than fail.
func fixtureNames(t *testing.T) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join("testdata", "*.abs"))
	if err != nil {
		t.Fatalf("globbing testdata: %v", err)
	}

	if len(matches) == 0 {
		t.Skip("no fixtures in testdata/ (not committed); skipping")
	}

	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, filepath.Base(m))
	}

	sort.Strings(names)

	return names
}

// openFixture opens a fixture by name, supplying fixturePassword for the
// encrypted ones so that their pages decrypt. Missing fixtures skip.
func openFixture(t *testing.T, name string) *File {
	t.Helper()

	path := requireFixture(t, name)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}

	if db.Encrypted() {
		db.Close()

		db, err = OpenWithPassword(path, fixturePassword)
		if err != nil {
			t.Fatalf("OpenWithPassword(%q): %v", name, err)
		}
	}

	t.Cleanup(func() { db.Close() })

	return db
}

// readerRecordIDs walks the record reader and reports where every row it
// yields came from.
//
// The Reader has no exported accessor for the position of the current row, so
// the test reads its unexported iteration state. That is deliberate: adding a
// public accessor is a production change and is not this test's to make.
func readerRecordIDs(r *Reader) ([]recordID, error) {
	var ids []recordID

	for r.Next() {
		ids = append(ids, recordID{
			PageNo: int32(r.dataPages[r.pageIdx]),
			ItemNo: uint16(r.recordIdx),
		})
	}

	return ids, r.Err()
}

// leafRecordIDs scans one index's leaf chain and reports the row references it
// carries.
func leafRecordIDs(ir *IndexReader, rootPageNo int) ([]recordID, error) {
	entries, err := ir.ScanIndex(rootPageNo)
	if err != nil {
		return nil, err
	}

	ids := make([]recordID, 0, len(entries))

	for _, e := range entries {
		pageNo, itemNo := e.RecordID()
		ids = append(ids, recordID{PageNo: pageNo, ItemNo: itemNo})
	}

	return ids, nil
}

// sortIDs orders record IDs so two sets can be compared element by element.
func sortIDs(ids []recordID) []recordID {
	out := make([]recordID, len(ids))
	copy(out, ids)

	sort.Slice(out, func(i, j int) bool {
		if out[i].PageNo != out[j].PageNo {
			return out[i].PageNo < out[j].PageNo
		}

		return out[i].ItemNo < out[j].ItemNo
	})

	return out
}

// diffIDs returns the IDs present in a but not in b, and vice versa.
func diffIDs(a, b []recordID) (onlyA, onlyB []recordID) {
	inB := make(map[recordID]int, len(b))
	for _, id := range b {
		inB[id]++
	}

	inA := make(map[recordID]int, len(a))
	for _, id := range a {
		inA[id]++
	}

	for _, id := range sortIDs(a) {
		if inB[id] == 0 && !slices.Contains(onlyA, id) {
			onlyA = append(onlyA, id)
		}
	}

	for _, id := range sortIDs(b) {
		if inA[id] == 0 && !slices.Contains(onlyB, id) {
			onlyB = append(onlyB, id)
		}
	}

	return onlyA, onlyB
}

// reportDiff logs at most limit differing IDs from each side.
func reportDiff(t *testing.T, label string, onlyA, onlyB []recordID) {
	t.Helper()

	const limit = 10

	if len(onlyA) > 0 {
		t.Errorf("%s: %d row(s) only in the record reader, first %d: %v",
			label, len(onlyA), min(limit, len(onlyA)), onlyA[:min(limit, len(onlyA))])
	}

	if len(onlyB) > 0 {
		t.Errorf("%s: %d row(s) only in the leaf scan, first %d: %v",
			label, len(onlyB), min(limit, len(onlyB)), onlyB[:min(limit, len(onlyB))])
	}
}

// TestOracleReaderMatchesLeafScan is the cross-check. For every fixture the
// set of rows the record reader produces must equal the set every user index's
// leaf chain reports — both in count and in the (PageNo, ItemNo) of each row.
func TestOracleReaderMatchesLeafScan(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			if unindexedFixtures[name] {
				t.Skip("fixture has no user index by design; see unindexedFixtures")
			}

			db := openFixture(t, name)

			reader, err := db.OpenTable()
			if err != nil {
				t.Fatalf("OpenTable: %v", err)
			}

			readerIDs, err := readerRecordIDs(reader)
			if err != nil {
				t.Fatalf("iterating records: %v", err)
			}

			ir, err := db.OpenIndex()
			if err != nil {
				t.Fatalf("OpenIndex: %v", err)
			}

			userIndexes := ir.UserIndexes()
			if len(userIndexes) == 0 {
				t.Fatalf("no user indexes found; cannot cross-check %d reader rows", len(readerIDs))
			}

			sortedReader := sortIDs(readerIDs)

			for _, idx := range userIndexes {
				label := fmt.Sprintf("index root %d (keySize %d)", idx.RootPageNo, idx.KeySize)

				leafIDs, err := leafRecordIDs(ir, idx.RootPageNo)
				if err != nil {
					t.Errorf("%s: ScanIndex: %v", label, err)

					continue
				}

				t.Logf("%s: reader %d rows, leaf scan %d entries", label, len(readerIDs), len(leafIDs))

				if len(leafIDs) != len(readerIDs) {
					t.Errorf("%s: leaf scan reports %d rows, record reader %d",
						label, len(leafIDs), len(readerIDs))
				}

				onlyReader, onlyLeaf := diffIDs(sortedReader, sortIDs(leafIDs))
				reportDiff(t, label, onlyReader, onlyLeaf)
			}
		})
	}
}

// TestOracleUserIndexesAgree checks the user indexes of a fixture against each
// other. They cover different columns but must reference the same rows.
func TestOracleUserIndexesAgree(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db := openFixture(t, name)

			ir, err := db.OpenIndex()
			if err != nil {
				t.Fatalf("OpenIndex: %v", err)
			}

			userIndexes := ir.UserIndexes()
			if len(userIndexes) < 2 {
				t.Skipf("only %d user index(es); nothing to compare", len(userIndexes))
			}

			first := userIndexes[0]

			baseline, err := leafRecordIDs(ir, first.RootPageNo)
			if err != nil {
				t.Fatalf("ScanIndex(%d): %v", first.RootPageNo, err)
			}

			sortedBaseline := sortIDs(baseline)

			for _, idx := range userIndexes[1:] {
				label := fmt.Sprintf("index root %d (keySize %d) vs root %d (keySize %d)",
					idx.RootPageNo, idx.KeySize, first.RootPageNo, first.KeySize)

				other, err := leafRecordIDs(ir, idx.RootPageNo)
				if err != nil {
					t.Errorf("%s: ScanIndex: %v", label, err)

					continue
				}

				if len(other) != len(baseline) {
					t.Errorf("%s: %d entries vs %d", label, len(other), len(baseline))
				}

				onlyOther, onlyBaseline := diffIDs(sortIDs(other), sortedBaseline)
				reportDiff(t, label, onlyOther, onlyBaseline)
			}
		})
	}
}

// TestOracleRowCounts pins the absolute row counts measured from the file
// bytes. The cross-check above catches a regression in either decoder; this
// catches one that moves both at once.
func TestOracleRowCounts(t *testing.T) {
	for _, name := range fixtureNames(t) {
		want, known := oracleRowCounts[name]
		if !known {
			continue
		}

		t.Run(name, func(t *testing.T) {
			db := openFixture(t, name)

			reader, err := db.OpenTable()
			if err != nil {
				t.Fatalf("OpenTable: %v", err)
			}

			ids, err := readerRecordIDs(reader)
			if err != nil {
				t.Fatalf("iterating records: %v", err)
			}

			if len(ids) != want {
				t.Errorf("record reader yields %d rows, want %d", len(ids), want)
			}

			ir, err := db.OpenIndex()
			if err != nil {
				t.Fatalf("OpenIndex: %v", err)
			}

			for _, idx := range ir.UserIndexes() {
				leafIDs, err := leafRecordIDs(ir, idx.RootPageNo)
				if err != nil {
					t.Errorf("index root %d: ScanIndex: %v", idx.RootPageNo, err)

					continue
				}

				if len(leafIDs) != want {
					t.Errorf("index root %d (keySize %d): leaf scan reports %d rows, want %d",
						idx.RootPageNo, idx.KeySize, len(leafIDs), want)
				}
			}
		})
	}
}

// TestOracleRowsPerDataPage pins the per-page row distribution of the fixtures
// GROUND_TRUTH §3 records it for. It is the layout derivation seen from the
// outside: a wrong recordsPerPage moves rows between pages even when the total
// happens to survive.
func TestOracleRowsPerDataPage(t *testing.T) {
	tests := []struct {
		fixture string
		perPage map[int32]int
	}{
		{"TS03.abs", map[int32]int{13: 18}},
		{"RREC0011.abs", map[int32]int{11: 23, 12: 7}},
		{"RR240011.abs", map[int32]int{12: 19, 13: 11}},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			db := openFixture(t, tt.fixture)

			reader, err := db.OpenTable()
			if err != nil {
				t.Fatalf("OpenTable: %v", err)
			}

			ids, err := readerRecordIDs(reader)
			if err != nil {
				t.Fatalf("iterating records: %v", err)
			}

			got := make(map[int32]int)
			for _, id := range ids {
				got[id.PageNo]++
			}

			if len(got) != len(tt.perPage) {
				t.Errorf("rows spread over %d data pages, want %d (%v)", len(got), len(tt.perPage), got)
			}

			for pageNo, want := range tt.perPage {
				if got[pageNo] != want {
					t.Errorf("page %d holds %d rows, want %d", pageNo, got[pageNo], want)
				}
			}
		})
	}
}

// TestOracleRecordLayout pins the layout GROUND_TRUTH §2 measured from the
// record stride and bitmap byte count observed in each file. The reader
// derives these from the schema alone, so agreement means the derivation
// matches the bytes.
func TestOracleRecordLayout(t *testing.T) {
	tests := []struct {
		fixture                                 string
		columns, nullFlagBytes, fieldDataSize   int
		recordSize, recordsPerPage, bitmapBytes int
	}{
		{"TS03.abs", 9, 2, 97, 99, 40, 5},
		{"RREC0011.abs", 20, 3, 173, 176, 23, 3},
		{"RCON0011.abs", 36, 5, 318, 323, 12, 2},
		{"RCFQ0011.abs", 7, 1, 46, 47, 86, 11},
		{"RMPA0011.abs", 31, 4, 282, 286, 14, 2},
		{"RR240011.abs", 27, 4, 204, 208, 19, 3},
		{"RRAD0011.abs", 12, 2, 135, 137, 29, 4},
		{"RRAI0011.abs", 12, 2, 121, 123, 32, 4},
		{"RPDG0011.abs", 5, 1, 24, 25, 161, 21},
		{"RFRQ0011.abs", 5, 1, 36, 37, 109, 14},
		{"RGRP0011.abs", 6, 1, 69, 70, 57, 8},
		{"RMND0011.abs", 3, 1, 16, 17, 236, 30},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			db := openFixture(t, tt.fixture)

			reader, err := db.OpenTable()
			if err != nil {
				t.Fatalf("OpenTable: %v", err)
			}

			checks := []struct {
				name      string
				got, want int
			}{
				{"columns", len(reader.Schema().Columns), tt.columns},
				{"nullFlagBytes", reader.nullFlagBytes, tt.nullFlagBytes},
				{"fieldDataSize", reader.fieldDataSize, tt.fieldDataSize},
				{"recordSize", reader.recordSize, tt.recordSize},
				{"recordsPerPage", reader.recordsPerPage, tt.recordsPerPage},
				{"bitmapBytes", reader.bitmapBytes, tt.bitmapBytes},
			}

			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
				}
			}
		})
	}
}
