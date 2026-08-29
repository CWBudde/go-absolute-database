package absdb

import (
	"encoding/binary"
	"fmt"
)

// Writing constraint records: the mirror of ddl_constraint.go's parser, and
// the half of that work the read path left open.
//
// Every schema operation until now copied the constraint array through
// verbatim. CREATE INDEX and DROP INDEX splice around it (spliceIndexRecord),
// DROP COLUMN refuses a column any record names, and CompactDatabase --  which
// writes a table's stream again from its definition rather than editing the
// one on disk -- refused a constrained table outright with
// ErrConstraintsNotRebuilt. That refusal is the whole reason this file exists:
// a rebuild has to emit the records, not preserve them.
//
// # The oracle
//
// There is no fixture of "the engine adding a constraint to an existing
// table", so byte identity comes from the other direction:
// TestConstraintRecordsReserializeByteForByte parses every constraint record
// in every fixture present and requires this serializer to reproduce the exact
// bytes it came from, rec.start to rec.end. That is 75 records over 27 tables,
// covering both bodies, all four kinds, one, two and three covered columns,
// and a CHECK record's present and absent bounds -- and it holds a serializer
// that agrees with this package's own reader but not with the engine to be a
// failure, because the bytes are the engine's.
//
// What it cannot cover is a field no record in the corpus varies: the body
// count is 1 everywhere, the reserved field is zero everywhere, and a typed
// value's absent marker never appears on a MINVALUE that the engine would have
// written differently. Those are written as the constants ddl_constraint.go
// already names, and a parsed record carrying anything else was refused before
// it ever reached here.

// serializeConstraintArray builds the int32-counted record array that follows
// the index records in a table's schema stream. A table with no constraints
// still has the array: a bare zero count, which is what every fresh CREATE
// TABLE writes.
func serializeConstraintArray(records []constraintRecord) ([]byte, error) {
	if len(records) > maxSchemaColumns {
		return nil, fmt.Errorf("%w: %d constraint records", ErrBadSchema, len(records))
	}

	out := binary.LittleEndian.AppendUint32(make([]byte, 0, 4+len(records)*64), uint32(len(records))) //nolint:gosec // bounded above

	for i, rec := range records {
		body, err := serializeConstraintRecord(rec)
		if err != nil {
			return nil, fmt.Errorf("constraint record %d: %w", i, err)
		}

		out = append(out, body...)
	}

	return out, nil
}

// serializeConstraintRecord builds one constraint record: the header every
// kind shares, then whichever of the two bodies its kind selects.
func serializeConstraintRecord(rec constraintRecord) ([]byte, error) {
	name, err := encodePascalName(rec.name)
	if err != nil {
		return nil, fmt.Errorf("name: %w", err)
	}

	out := make([]byte, 0, 1+1+len(name)+4+constraintReservedSize+4+64)
	out = append(out, byte(rec.kind))
	out = appendPascalString(out, name)
	out = binary.LittleEndian.AppendUint32(out, rec.objectID)
	out = append(out, make([]byte, constraintReservedSize)...)
	out = binary.LittleEndian.AppendUint32(out, rec.ownerID)

	switch rec.kind {
	case constraintPrimaryKey, constraintUnique:
		out, err = appendKeyConstraintBody(out, rec)
	case constraintNotNull, constraintCheck:
		out, err = appendColumnConstraintBody(out, rec)
	default:
		return nil, fmt.Errorf("unsupported constraint kind %d", byte(rec.kind))
	}

	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", rec.kind, rec.name, err)
	}

	return out, nil
}

// appendKeyConstraintBody writes the body of a PRIMARY KEY or UNIQUE record.
// Both the table and the index it names are required: a key constraint that
// names no index is one the engine has never written, and an empty name would
// serialize to a zero length byte that parseKeyConstraintBody would then read
// back as a nameless index.
func appendKeyConstraintBody(out []byte, rec constraintRecord) ([]byte, error) {
	if len(rec.columns) > maxSchemaColumns {
		return nil, fmt.Errorf("%d covered columns", len(rec.columns))
	}

	out = binary.LittleEndian.AppendUint32(out, constraintBodyCount)
	out = append(out, 0) // pad

	// The table name is the one field the engine ever leaves empty: a UNIQUE
	// record CREATE UNIQUE INDEX wrote carries none, where the same field of a
	// CREATE TABLE ... PRIMARY KEY record names the table
	// (testdata/Keys-uniqidx.abs against testdata/Keys.abs).
	out, err := appendOptionalSizedString32(out, rec.table)
	if err != nil {
		return nil, fmt.Errorf("table name: %w", err)
	}

	out, err = appendSizedString32(out, rec.index)
	if err != nil {
		return nil, fmt.Errorf("index name: %w", err)
	}

	out = binary.LittleEndian.AppendUint32(out, uint32(len(rec.columns))) //nolint:gosec // bounded above

	for i, col := range rec.columns {
		out = binary.LittleEndian.AppendUint32(out, col.objectID)

		out, err = appendSizedString32(out, col.name)
		if err != nil {
			return nil, fmt.Errorf("column %d: %w", i, err)
		}
	}

	return out, nil
}

// appendColumnConstraintBody writes the body of a NOT NULL or CHECK record:
// the same two names sized with single bytes, and the two bounds a CHECK
// record adds. Exactly one covered column is required, because the record has
// nowhere to put a second one -- a column-shaped record names its column once,
// and identifies it again through the ownerObjectID in the header.
func appendColumnConstraintBody(out []byte, rec constraintRecord) ([]byte, error) {
	if len(rec.columns) != 1 {
		return nil, fmt.Errorf("%d covered columns, want exactly 1", len(rec.columns))
	}

	out = append(out, constraintBodyCount, 0) // count, pad

	out, err := appendSizedString8(out, rec.table)
	if err != nil {
		return nil, fmt.Errorf("table name: %w", err)
	}

	out, err = appendSizedString8(out, rec.columns[0].name)
	if err != nil {
		return nil, fmt.Errorf("column name: %w", err)
	}

	if rec.kind != constraintCheck {
		return out, nil
	}

	out, err = appendTypedValue(out, rec.minValue)
	if err != nil {
		return nil, fmt.Errorf("minimum value: %w", err)
	}

	out, err = appendTypedValue(out, rec.maxValue)
	if err != nil {
		return nil, fmt.Errorf("maximum value: %w", err)
	}

	return out, nil
}

// appendTypedValue writes a base type, the present/absent flag and, when
// present, the int32-counted payload readTypedValue reads back.
func appendTypedValue(out []byte, v typedValue) ([]byte, error) {
	out = append(out, byte(v.baseType))

	if !v.present {
		return append(out, typedValueAbsent), nil
	}

	if len(v.data) > maxTypedValueSize {
		return nil, fmt.Errorf("%w: %d-byte value", ErrValueRange, len(v.data))
	}

	out = append(out, typedValuePresent)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(v.data))) //nolint:gosec // bounded above

	return append(out, v.data...), nil
}

// appendPascalString writes an already-encoded name as a length byte and its
// bytes. The caller encodes through encodePascalName, which is what bounds the
// length to what the single byte can describe.
func appendPascalString(out, raw []byte) []byte {
	return append(append(out, byte(len(raw))), raw...) //nolint:gosec // encodePascalName bounds raw to 255 bytes
}

// appendSizedString32 writes the int32 byte count and the Pascal string it
// introduces, with the count including the string's own length byte -- the
// redundancy readSizedStringBody checks rather than skips.
func appendSizedString32(out []byte, name string) ([]byte, error) {
	raw, err := encodePascalName(name)
	if err != nil {
		return nil, err
	}

	return appendSized32(out, raw), nil
}

// appendOptionalSizedString32 is appendSizedString32 for a field the engine
// writes empty: it accepts the empty string and writes "01 00 00 00 00", the
// size field plus a zero length byte, which is what the engine wrote for the
// table name of testdata/Keys-uniqidx.abs's UNIQUE record.
func appendOptionalSizedString32(out []byte, name string) ([]byte, error) {
	raw, err := encodeOptionalPascalName(name)
	if err != nil {
		return nil, err
	}

	return appendSized32(out, raw), nil
}

// appendSized32 writes an already-encoded name behind its int32 size field.
// The size counts the Pascal string including its own length byte, which is
// the redundancy parseConstraintRecord checks rather than skips.
func appendSized32(out, raw []byte) []byte {
	out = binary.LittleEndian.AppendUint32(out, uint32(len(raw)+1)) //nolint:gosec // encodePascalName bounds raw to 255 bytes

	return appendPascalString(out, raw)
}

// appendSizedString8 is appendSizedString32 with the single-byte count NOT
// NULL and CHECK records use.
func appendSizedString8(out []byte, name string) ([]byte, error) {
	raw, err := encodePascalName(name)
	if err != nil {
		return nil, err
	}

	out = append(out, byte(len(raw)+1)) //nolint:gosec // encodePascalName bounds raw to 255 bytes

	return appendPascalString(out, raw), nil
}
