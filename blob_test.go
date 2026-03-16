package absdb

import (
	"testing"
)

func TestRPDG0011ReadBlobs(t *testing.T) {
	db := openTestFile(t, "RPDG0011.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	schema := reader.Schema()

	// Find BLOB columns.
	var blobCols []int
	for i, c := range schema.Columns {
		if c.IsBLOB() {
			blobCols = append(blobCols, i)
			t.Logf("BLOB column %d: %s (%s)", i, c.Name, c.FieldType)
		}
	}

	if len(blobCols) == 0 {
		t.Fatal("expected BLOB columns in RPDG0011")
	}

	count := 0
	blobCount := 0

	for reader.Next() {
		rec := reader.Record()
		count++

		for _, col := range blobCols {
			if rec.IsNull(col) {
				continue
			}

			ref := rec.BlobRef(col)
			if ref.IsNull() {
				continue
			}

			data, err := rec.Blob(col)
			if err != nil {
				t.Fatalf("record %d, col %d (%s): Blob() error: %v",
					count, col, schema.Columns[col].Name, err)
			}

			if len(data) == 0 {
				t.Errorf("record %d, col %d: BLOB ref page=%d item=%d but data is empty",
					count, col, ref.PageNo, ref.ItemNo)
			}

			if count <= 3 {
				t.Logf("record %d, col %d (%s): page=%d item=%d, %d bytes",
					count, col, schema.Columns[col].Name, ref.PageNo, ref.ItemNo, len(data))
			}

			blobCount++
		}
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("iteration error: %v", err)
	}

	t.Logf("Read %d records, %d non-null BLOBs", count, blobCount)

	if blobCount == 0 {
		t.Error("expected at least 1 non-null BLOB")
	}
}

func TestBlobRefNull(t *testing.T) {
	ref := BlobRef{PageNo: 0, ItemNo: 0}
	if !ref.IsNull() {
		t.Error("zero BlobRef should be null")
	}

	ref = BlobRef{PageNo: 5, ItemNo: 0}
	if ref.IsNull() {
		t.Error("non-zero PageNo BlobRef should not be null")
	}
}

func TestReadBlobNullRef(t *testing.T) {
	db := openTestFile(t, "RPDG0011.abs")

	ref := BlobRef{PageNo: 0, ItemNo: 0}

	data, err := db.ReadBlob(ref)
	if err != nil {
		t.Fatalf("ReadBlob(null ref): %v", err)
	}

	if data != nil {
		t.Error("expected nil data for null ref")
	}
}

func TestReadBlobInvalidPage(t *testing.T) {
	db := openTestFile(t, "RPDG0011.abs")

	ref := BlobRef{PageNo: 999, ItemNo: 0}

	_, err := db.ReadBlob(ref)
	if err == nil {
		t.Error("expected error for invalid page")
	}
}

func TestTS03NullBlobs(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	// TS03 has BLOB columns but all are NULL.
	for reader.Next() {
		rec := reader.Record()

		for i, c := range reader.Schema().Columns {
			if !c.IsBLOB() {
				continue
			}

			data, err := rec.Blob(i)
			if err != nil {
				t.Fatalf("unexpected error reading null BLOB: %v", err)
			}

			if data != nil {
				t.Errorf("col %d (%s): expected nil for null BLOB, got %d bytes",
					i, c.Name, len(data))
			}
		}
	}

	if err := reader.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRPDG0011BlobSizes(t *testing.T) {
	db := openTestFile(t, "RPDG0011.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	// Read first record's PDiaZB1 and PDiaZB2.
	if !reader.Next() {
		t.Fatal("no records")
	}

	rec := reader.Record()

	// Column 3: PDiaZB1, Column 4: PDiaZB2 - both should have data.
	for _, col := range []int{3, 4} {
		if rec.IsNull(col) {
			t.Errorf("col %d: expected non-null", col)
			continue
		}

		data, err := rec.Blob(col)
		if err != nil {
			t.Fatalf("col %d: %v", col, err)
		}

		// Each BLOB contains float32 triplets (x, y, z/weight).
		// Sizes should be multiples of 12 (3 × 4 bytes).
		if len(data)%12 != 0 {
			t.Errorf("col %d: BLOB size %d not a multiple of 12", col, len(data))
		}

		numPoints := len(data) / 12
		t.Logf("col %d (%s): %d bytes = %d float32 triplets",
			col, reader.Schema().Columns[col].Name, len(data), numPoints)

		if numPoints < 10 {
			t.Errorf("col %d: expected at least 10 points, got %d", col, numPoints)
		}
	}
}
