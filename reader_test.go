package absdb

import (
	"errors"
	"math"
	"testing"
)

func TestTS03ReadRecords(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	var records []Record
	for reader.Next() {
		records = append(records, reader.Record())
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("iteration error: %v", err)
	}

	t.Logf("Read %d records from TS03.abs", len(records))

	if len(records) == 0 {
		t.Fatal("expected at least 1 record")
	}

	// Verify first record.
	rec := records[0]
	schema := reader.Schema()

	// Column 0: ZugArt (AutoInc) - should be 1.
	autoInc := rec.Uint32(0)
	if autoInc != 1 {
		t.Errorf("record 0 ZugArt = %d, want 1", autoInc)
	}

	// Column 1: Name (String/40) - first train type name.
	name := rec.String(1)
	t.Logf("Record 0: AutoInc=%d, Name=%q", autoInc, name)

	if name == "" {
		t.Error("record 0 Name is empty")
	}

	// Column 2: SBA (Double) - should be a reasonable float.
	sba := rec.Float(2)
	t.Logf("Record 0: SBA=%f", sba)

	if math.IsNaN(sba) || math.IsInf(sba, 0) {
		t.Errorf("record 0 SBA = %f, expected a finite number", sba)
	}

	// Log all records for debugging.
	for i, r := range records {
		ai := r.Uint32(0)
		n := r.String(1)
		s := r.Float(2)
		t.Logf("  [%2d] AutoInc=%d Name=%-25q SBA=%.1f", i, ai, n, s)

		_ = schema
	}
}

func TestTS03RecordCount(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatal(err)
	}

	count := 0

	for reader.Next() {
		reader.Record()

		count++
	}

	t.Logf("TS03 record count: %d", count)

	if count < 15 {
		t.Errorf("expected at least 15 records, got %d", count)
	}
}

func TestRREC0011ReadRecords(t *testing.T) {
	db := openTestFile(t, "RREC0011.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	var records []Record
	for reader.Next() {
		records = append(records, reader.Record())
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("iteration error: %v", err)
	}

	t.Logf("Read %d records from RREC0011.abs", len(records))

	if len(records) == 0 {
		t.Fatal("expected at least 1 record")
	}

	// Column 5: Name (String/30) - receiver name.
	// Column 10: X/m (Double) - X coordinate.
	// Column 11: Y/m (Double) - Y coordinate.
	rec := records[0]
	name := rec.String(5)
	x := rec.Float(10)
	y := rec.Float(11)
	t.Logf("Record 0: Name=%q, X=%.2f, Y=%.2f", name, x, y)

	if name == "" {
		t.Error("record 0 Name is empty")
	}

	// X and Y should be reasonable coordinates (> 100).
	if x < 100 || y < 100 {
		t.Errorf("record 0 X=%.2f, Y=%.2f: expected reasonable coordinates", x, y)
	}

	// Log first few records.
	for i, r := range records {
		if i >= 5 {
			break
		}

		recno := r.Int(0)
		n := r.String(5)
		xv := r.Float(10)
		yv := r.Float(11)
		t.Logf("  [%2d] RecNo=%d Name=%-25q X=%.2f Y=%.2f", i, recno, n, xv, yv)
	}
}

func TestAddressesNoData(t *testing.T) {
	db := openTestFile(t, "Addresses.abs")

	_, err := db.OpenTable()
	if !errors.Is(err, ErrNoData) {
		t.Errorf("expected ErrNoData, got %v", err)
	}
}
