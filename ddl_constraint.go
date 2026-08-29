package absdb

import (
	"errors"
	"fmt"
	"strings"
)

// Constraint records: the second array in a table's column-definition stream.
//
// This is the region parseSchemaTail used to refuse outright. Every table
// carrying a NOT NULL, PRIMARY KEY, UNIQUE or MINVALUE/MAXVALUE clause has one,
// which is nearly every real customer table, so the refusal took CREATE INDEX,
// DROP INDEX, DROP COLUMN and index maintenance with it.
//
// # How it was decoded
//
// testdata/Constraints.abs is a database of twelve two-column tables, each one
// clause away from the control CNone (A INTEGER, B VARCHAR(10)). Subtracting
// the control's schema stream from another table's isolates exactly one record,
// which is what every offset below comes from:
//
//	CNone      the control: no constraints, no indexes
//	CNotNull   A INTEGER NOT NULL                     -> kind 3
//	CPk        A INTEGER PRIMARY KEY                  -> kind 0, plus an index
//	CUnique    A INTEGER UNIQUE                       -> kind 2, plus an index
//	CDefault   A INTEGER DEFAULT 7                    -> no record at all; the
//	                                                     default lives in the
//	                                                     column definition
//	                                                     (see schema.go's
//	                                                     findColumnTerminator)
//	CMinMax    A INTEGER MINVALUE 0 MAXVALUE 99       -> kind 4, two values
//	CBoth      A NOT NULL, B VARCHAR(10) UNIQUE       -> kinds 3 and 2 together
//	CPkMulti   PRIMARY KEY (A, B)                     -> kind 0, two columns
//	CIdx*      four CREATE INDEX shapes               -> index records only
//
// The object ids in that file corroborate the reading: each table hands out one
// id for itself, one per column, one per index, and one per constraint record,
// in that order, which is exactly why the ids run 1, 4, 8, 13, 18, 21, 25, 31,
// 36, 39, 42, 45 rather than three apart.
//
// # Layout
//
// The array is a count and that many records. Every record opens the same way:
//
//	byte    kind                      0 PRIMARY KEY, 2 UNIQUE, 3 NOT NULL,
//	                                  4 CHECK (MINVALUE/MAXVALUE). 1 is not
//	                                  observed anywhere and is refused.
//	Pascal  name                      "C_PK$SrcNo$RecNo$Floor",
//	                                  "$C_NotNull$RCON0011.abs$SrcNo", ...
//	int32   objectID                  database-wide, from the same sequence
//	                                  tables, columns and indexes draw from
//	8 bytes reserved                  zero in all 66 records in the corpus
//	int32   ownerObjectID             the object the constraint hangs off: the
//	                                  covered column for kinds 3 and 4, the
//	                                  index implementing the key for 0 and 2
//
// The body then splits in two, and the split is the one surprise in the format:
// key-shaped records (0, 2) size their strings and counts with int32 fields,
// column-shaped records (3, 4) size the same fields with single bytes. Both
// start with a count of 1 and a zero pad byte; what the count counts is not
// established, only that it is 1 in every observed record.
//
// Key-shaped (kind 0, 2):
//
//	int32   count                     1
//	byte    pad                       0
//	int32+Pascal  tableName
//	int32+Pascal  indexName           the index record implementing this key
//	int32   columnCount
//	  int32       columnObjectID
//	  int32+Pascal columnName
//
// Column-shaped (kind 3, 4):
//
//	byte    count                     1
//	byte    pad                       0
//	byte+Pascal   tableName
//	byte+Pascal   columnName
//	kind 4 only: two typed values, MINVALUE then MAXVALUE
//
// The int32/byte size fields count the Pascal string including its own length
// byte, so "CPk" is written as 04 00 00 00 03 'C' 'P' 'k'. That redundancy is
// checked rather than skipped: it is what makes a mis-parse fail loudly instead
// of sliding.
//
// # What is not decoded
//
// Kind 1, a non-zero reserved field, a count other than 1, and a non-zero pad
// byte are all refused with ErrSchemaTailNotUnderstood rather than guessed at.
// A wrong guess here rewrites a real customer database, and the corpus is the
// only evidence there is.

// constraintKind identifies which clause of the SDK manual's CREATE TABLE
// grammar produced a constraint record.
type constraintKind byte

const (
	// constraintPrimaryKey is a PRIMARY KEY clause, table-level or per-column.
	// It always comes with an index record the constraint names.
	constraintPrimaryKey constraintKind = 0

	// constraintUnique is a UNIQUE clause. Like a primary key, it is backed by
	// an index record.
	constraintUnique constraintKind = 2

	// constraintNotNull is a NOT NULL clause on one column.
	constraintNotNull constraintKind = 3

	// constraintCheck is a MINVALUE/MAXVALUE pair on one column. DBManager
	// calls the record "$C_Check$<table>$<column>" and stores both bounds as
	// typed values, either of which may be absent.
	constraintCheck constraintKind = 4
)

// String names a constraint kind for error messages.
func (k constraintKind) String() string {
	switch k {
	case constraintPrimaryKey:
		return "PRIMARY KEY"
	case constraintUnique:
		return "UNIQUE"
	case constraintNotNull:
		return "NOT NULL"
	case constraintCheck:
		return "CHECK"
	default:
		return fmt.Sprintf("constraintKind(%d)", byte(k))
	}
}

const (
	// constraintReservedSize is the width of the zero field between a
	// constraint record's objectID and its ownerObjectID. All 66 records in
	// the corpus have it zero, and a non-zero one is refused.
	constraintReservedSize = 8

	// constraintBodyCount is the leading count both record bodies carry. Only
	// 1 is observed.
	constraintBodyCount = 1

	// typedValueAbsent marks a typed value that is not set: no size and no
	// data follow. This is what a column with no DEFAULT and a CHECK record
	// with no MINVALUE both store.
	typedValueAbsent = 0xFF

	// typedValuePresent marks a typed value whose int32 size and payload
	// follow.
	typedValuePresent = 0x00
)

// constraintColumn is one column a constraint covers. objectID is carried only
// by key-shaped records; column-shaped ones name their single column in the
// record's ownerObjectID instead, and leave this zero.
type constraintColumn struct {
	name     string
	objectID uint32
}

// constraintRecord is one parsed entry of the schema stream's constraint array.
// start and end are its byte range within the decompressed stream, so a caller
// can splice around it the way indexRecord's are used.
type constraintRecord struct {
	kind       constraintKind
	name       string
	objectID   uint32
	ownerID    uint32
	table      string
	index      string
	columns    []constraintColumn
	minValue   typedValue
	maxValue   typedValue
	start, end int
}

// namesColumn reports whether this constraint covers a column of that name,
// case-insensitively like every other name lookup in this package. It is what
// lets DropColumn refuse precisely instead of refusing on any constrained
// table.
func (c constraintRecord) namesColumn(column string) bool {
	for _, col := range c.columns {
		if strings.EqualFold(col.name, column) {
			return true
		}
	}

	return false
}

// typedValue is the format's optional scalar: a base type, a present/absent
// flag, and the raw little-endian bytes when present. The same three fields
// encode a column's DEFAULT clause (schema.go) and a CHECK record's MINVALUE
// and MAXVALUE, which is why they share one reader.
type typedValue struct {
	baseType BaseFieldType
	present  bool
	data     []byte
}

// parseConstraintRecords parses count consecutive constraint records starting
// at pos.
func parseConstraintRecords(data []byte, pos, count int) ([]constraintRecord, int, error) {
	records := make([]constraintRecord, 0, count)

	for i := range count {
		rec, next, err := parseConstraintRecord(data, pos)
		if err != nil {
			return nil, 0, fmt.Errorf("record %d: %w", i, err)
		}

		rec.start, rec.end = pos, next
		records = append(records, rec)
		pos = next
	}

	return records, pos, nil
}

// parseConstraintRecord parses one constraint record: the header every kind
// shares, then whichever of the two bodies its kind selects.
func parseConstraintRecord(data []byte, pos int) (constraintRecord, int, error) {
	if pos >= len(data) {
		return constraintRecord{}, 0, errors.New("truncated kind")
	}

	rec := constraintRecord{kind: constraintKind(data[pos])}
	pos++

	var err error

	rec.name, pos, err = readPascalString(data, pos)
	if err != nil {
		return constraintRecord{}, 0, fmt.Errorf("name: %w", err)
	}

	rec.objectID, pos, err = readUint32(data, pos, "objectId")
	if err != nil {
		return constraintRecord{}, 0, err
	}

	pos, err = skipZeroBytes(data, pos, constraintReservedSize, "reserved field")
	if err != nil {
		return constraintRecord{}, 0, err
	}

	rec.ownerID, pos, err = readUint32(data, pos, "owner object id")
	if err != nil {
		return constraintRecord{}, 0, err
	}

	switch rec.kind {
	case constraintPrimaryKey, constraintUnique:
		pos, err = parseKeyConstraintBody(&rec, data, pos)
	case constraintNotNull, constraintCheck:
		pos, err = parseColumnConstraintBody(&rec, data, pos)
	default:
		return constraintRecord{}, 0, fmt.Errorf("unsupported constraint kind %d", byte(rec.kind))
	}

	if err != nil {
		return constraintRecord{}, 0, fmt.Errorf("%s %q: %w", rec.kind, rec.name, err)
	}

	return rec, pos, nil
}

// parseKeyConstraintBody reads the body of a PRIMARY KEY or UNIQUE record: the
// table it belongs to, the index implementing it, and the covered columns, all
// sized with int32 fields.
func parseKeyConstraintBody(rec *constraintRecord, data []byte, pos int) (int, error) {
	count, pos, err := readUint32(data, pos, "body count")
	if err != nil {
		return 0, err
	}

	if count != constraintBodyCount {
		return 0, fmt.Errorf("body count = %d, want %d", count, constraintBodyCount)
	}

	pos, err = skipZeroBytes(data, pos, 1, "pad byte")
	if err != nil {
		return 0, err
	}

	rec.table, pos, err = readSizedString32(data, pos, "table name")
	if err != nil {
		return 0, err
	}

	rec.index, pos, err = readSizedString32(data, pos, "index name")
	if err != nil {
		return 0, err
	}

	columnCount, pos, err := readUint32(data, pos, "column count")
	if err != nil {
		return 0, err
	}

	if columnCount > maxSchemaColumns || int(columnCount) > len(data)-pos {
		return 0, fmt.Errorf("column count %d exceeds %d remaining bytes", columnCount, len(data)-pos)
	}

	rec.columns = make([]constraintColumn, 0, columnCount)

	for i := range int(columnCount) {
		var col constraintColumn

		col.objectID, pos, err = readUint32(data, pos, "column object id")
		if err != nil {
			return 0, fmt.Errorf("column %d: %w", i, err)
		}

		col.name, pos, err = readSizedString32(data, pos, "column name")
		if err != nil {
			return 0, fmt.Errorf("column %d: %w", i, err)
		}

		rec.columns = append(rec.columns, col)
	}

	return pos, nil
}

// parseColumnConstraintBody reads the body of a NOT NULL or CHECK record: the
// table and the single covered column, sized with single bytes, plus the two
// bounds a CHECK record carries.
func parseColumnConstraintBody(rec *constraintRecord, data []byte, pos int) (int, error) {
	if pos >= len(data) {
		return 0, errors.New("truncated body count")
	}

	if count := data[pos]; count != constraintBodyCount {
		return 0, fmt.Errorf("body count = %d, want %d", count, constraintBodyCount)
	}

	pos, err := skipZeroBytes(data, pos+1, 1, "pad byte")
	if err != nil {
		return 0, err
	}

	rec.table, pos, err = readSizedString8(data, pos, "table name")
	if err != nil {
		return 0, err
	}

	name, pos, err := readSizedString8(data, pos, "column name")
	if err != nil {
		return 0, err
	}

	rec.columns = []constraintColumn{{name: name}}

	if rec.kind != constraintCheck {
		return pos, nil
	}

	rec.minValue, pos, err = readTypedValue(data, pos)
	if err != nil {
		return 0, fmt.Errorf("minimum value: %w", err)
	}

	rec.maxValue, pos, err = readTypedValue(data, pos)
	if err != nil {
		return 0, fmt.Errorf("maximum value: %w", err)
	}

	return pos, nil
}

// readTypedValue reads a base type, a present/absent flag and, when present, an
// int32-counted payload. A flag that is neither of the two observed values is
// an error rather than a skip: it is the anchor that keeps a mis-parse from
// running on.
func readTypedValue(data []byte, pos int) (typedValue, int, error) {
	if pos+2 > len(data) {
		return typedValue{}, 0, errors.New("truncated typed value header")
	}

	v := typedValue{baseType: BaseFieldType(data[pos])}
	flag := data[pos+1]
	pos += 2

	switch flag {
	case typedValueAbsent:
		return v, pos, nil
	case typedValuePresent:
		size, pos, err := readUint32(data, pos, "typed value size")
		if err != nil {
			return typedValue{}, 0, err
		}

		if int64(size) > int64(len(data)-pos) {
			return typedValue{}, 0, fmt.Errorf("typed value of %d bytes exceeds %d remaining", size, len(data)-pos)
		}

		v.present = true
		v.data = data[pos : pos+int(size)]

		return v, pos + int(size), nil
	default:
		return typedValue{}, 0, fmt.Errorf("typed value flag = %#x", flag)
	}
}

// readSizedString32 reads an int32 byte count followed by a Pascal string, and
// requires the count to agree with the string it introduces.
func readSizedString32(data []byte, pos int, field string) (string, int, error) {
	size, pos, err := readUint32(data, pos, field+" size")
	if err != nil {
		return "", 0, err
	}

	return readSizedStringBody(data, pos, int64(size), field)
}

// readSizedString8 is readSizedString32 with a single-byte count, the width
// NOT NULL and CHECK records use.
func readSizedString8(data []byte, pos int, field string) (string, int, error) {
	if pos >= len(data) {
		return "", 0, fmt.Errorf("truncated %s size", field)
	}

	return readSizedStringBody(data, pos+1, int64(data[pos]), field)
}

// readSizedStringBody reads the Pascal string a size field introduces and
// checks that the two agree: size counts the length byte as well as the
// characters.
func readSizedStringBody(data []byte, pos int, size int64, field string) (string, int, error) {
	s, next, err := readPascalString(data, pos)
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w", field, err)
	}

	if size != int64(next-pos) {
		return "", 0, fmt.Errorf("%s size = %d, but the string occupies %d bytes", field, size, next-pos)
	}

	return s, next, nil
}

// skipZeroBytes advances pos by n and requires every byte skipped to be zero.
// Unlike skipBytes it refuses rather than ignores: a non-zero byte in a field
// the corpus only ever shows as zero is data this package does not understand.
func skipZeroBytes(data []byte, pos, n int, field string) (int, error) {
	if pos+n > len(data) {
		return 0, fmt.Errorf("truncated %s", field)
	}

	for i := pos; i < pos+n; i++ {
		if data[i] != 0 {
			return 0, fmt.Errorf("%s byte %d = %#x, want 0", field, i-pos, data[i])
		}
	}

	return pos + n, nil
}
