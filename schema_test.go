package absdb

import (
	"testing"
)

func TestTS03Schema(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	schema, err := db.Schema()
	if err != nil {
		t.Fatalf("Schema(): %v", err)
	}

	// TS03 has 9 columns in its schema definition.
	if len(schema.Columns) != 9 {
		t.Fatalf("len(Columns) = %d, want 9", len(schema.Columns))
	}

	// Verify known columns.
	expected := []struct {
		name      string
		fieldType FieldType
		baseType  BaseFieldType
		size      uint32
	}{
		{"ZugArt", FieldAutoInc, BftInt32, 0},
		{"Name", FieldString, BftVarchar, 40},
		{"SBA", FieldDouble, BftDouble, 0},
		{"Vmax", FieldDouble, BftDouble, 0},
		{"LZug", FieldDouble, BftDouble, 0},
		{"DFz", FieldDouble, BftDouble, 0},
		{"DAo", FieldDouble, BftDouble, 0},
		{"Kommentar", FieldMemo, BftClob, 0},
		{"Graphic", FieldGraphic, BftBlob, 0},
	}

	for i, want := range expected {
		col := schema.Columns[i]
		if col.Name != want.name {
			t.Errorf("col %d: Name = %q, want %q", i, col.Name, want.name)
		}

		if col.FieldType != want.fieldType {
			t.Errorf("col %d %q: FieldType = %v, want %v", i, col.Name, col.FieldType, want.fieldType)
		}

		if col.BaseType != want.baseType {
			t.Errorf("col %d %q: BaseType = %d, want %d", i, col.Name, col.BaseType, want.baseType)
		}

		if col.Size != want.size {
			t.Errorf("col %d %q: Size = %d, want %d", i, col.Name, col.Size, want.size)
		}

		if col.Position != i {
			t.Errorf("col %d %q: Position = %d, want %d", i, col.Name, col.Position, i)
		}
	}
}

func TestAddressesSchema(t *testing.T) {
	db := openTestFile(t, "Addresses.abs")

	schema, err := db.Schema()
	if err != nil {
		t.Fatalf("Schema(): %v", err)
	}

	// Log all columns for debugging.
	for i, col := range schema.Columns {
		t.Logf("Col %2d: %-20s type=%-12s base=%2d size=%d",
			i, col.Name, col.FieldType, col.BaseType, col.Size)
	}

	// Verify first few known columns.
	if schema.Columns[0].Name != "Eintrag" {
		t.Errorf("col 0 Name = %q, want Eintrag", schema.Columns[0].Name)
	}

	if schema.Columns[0].FieldType != FieldAutoInc {
		t.Errorf("col 0 FieldType = %v, want AutoInc", schema.Columns[0].FieldType)
	}

	if schema.Columns[1].Name != "Company" {
		t.Errorf("col 1 Name = %q, want Company", schema.Columns[1].Name)
	}

	if schema.Columns[1].FieldType != FieldString {
		t.Errorf("col 1 FieldType = %v, want String", schema.Columns[1].FieldType)
	}

	if schema.Columns[1].Size != 128 {
		t.Errorf("col 1 Size = %d, want 128", schema.Columns[1].Size)
	}
}

func TestRREC0011Schema(t *testing.T) {
	db := openTestFile(t, "RREC0011.abs")

	schema, err := db.Schema()
	if err != nil {
		t.Fatalf("Schema(): %v", err)
	}

	// Log all columns.
	for i, col := range schema.Columns {
		t.Logf("Col %2d: %-20s type=%-12s base=%2d size=%d",
			i, col.Name, col.FieldType, col.BaseType, col.Size)
	}

	if len(schema.Columns) == 0 {
		t.Fatal("expected at least 1 column")
	}
}

func TestColumnIsBLOB(t *testing.T) {
	tests := []struct {
		baseType BaseFieldType
		want     bool
	}{
		{BftBlob, true},
		{BftClob, true},
		{BftWideClob, true},
		{BftVarchar, false},
		{BftInt32, false},
		{BftDouble, false},
	}

	for _, tt := range tests {
		col := Column{BaseType: tt.baseType}
		if got := col.IsBLOB(); got != tt.want {
			t.Errorf("IsBLOB() for baseType %d = %v, want %v", tt.baseType, got, tt.want)
		}
	}
}

func TestFieldTypeString(t *testing.T) {
	tests := []struct {
		ft   FieldType
		want string
	}{
		{FieldString, "String"},
		{FieldInteger, "Integer"},
		{FieldDouble, "Double"},
		{FieldAutoInc, "AutoInc"},
		{FieldMemo, "Memo"},
		{FieldBLOB, "BLOB"},
		{FieldGUID, "GUID"},
	}

	for _, tt := range tests {
		if got := tt.ft.String(); got != tt.want {
			t.Errorf("FieldType(%d).String() = %q, want %q", int(tt.ft), got, tt.want)
		}
	}
}
