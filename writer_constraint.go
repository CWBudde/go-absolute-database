package absdb

import (
	"errors"
	"fmt"
)

// Checking constraint records: the other half of the gap ddl_constraint.go
// opened by making them readable.
//
// A table declaring any constraint refused every write, because nothing here
// checked one and a write that ignores a NOT NULL leaves the file holding a
// row the engine would have rejected. That blanket refusal is now narrowed to
// what is still unchecked. Two of the four kinds are checked here:
//
//	NOT NULL (kind 3)   the record's null flag for the covered column
//	CHECK    (kind 4)   the value against the MINVALUE/MAXVALUE pair
//
// and the two key kinds -- PRIMARY KEY (0) and UNIQUE (2) -- are checked by
// their index rather than here. A key record carries nothing to test a row
// against: it names the index implementing the key, and it is that index that
// refuses a duplicate (checkKeyIndexes in writer_index.go). So what this file
// does for a key is structural -- it establishes that the index exists, that
// it is one this writer maintains, and that it is flagged UNIQUE or PRIMARY --
// and then records no per-row check at all. A key whose index fails any of
// that keeps the refusal, because checking a constraint while leaving its
// index stale is the exact failure Phase 7's unguarded Update was.
//
// # What a check is measured against
//
// Nothing, directly: no fixture shows the engine rejecting a write, because a
// rejected write leaves no file behind. So these checks are held to the
// narrower standard of never passing a row the constraint forbids, and to
// refusing rather than guessing wherever the engine's own rule is unknown.
// Two such rules are assumed and recorded in docs/open-questions.md:
//
//   - a NULL passes a CHECK constraint, which is what SQL says and what a
//     nullable MINVALUE column would otherwise make unwritable;
//   - a bound is inclusive, so MINVALUE 0 admits 0.
//
// Both only ever make this package accept a row; neither can make it write one
// the constraint's own bytes say is out of range.

var (
	// ErrNotNullViolated reports a write storing NULL in a column a NOT NULL
	// constraint record covers, or in one a PRIMARY KEY index covers. The
	// second is not redundant: a PRIMARY KEY column carries no NOT NULL record
	// and the engine refuses a NULL in it anyway (testdata/README.md).
	ErrNotNullViolated = errors.New("absdb: NULL in a NOT NULL column")

	// ErrCheckViolated reports a write storing a value outside a column's
	// MINVALUE/MAXVALUE pair.
	ErrCheckViolated = errors.New("absdb: value outside a column's MINVALUE/MAXVALUE bounds")
)

// constraintChecks is a table's constraint array reduced to what a write has
// to test, resolved once per writer against the schema so that a name lookup
// and a type check do not repeat per row.
//
// The zero value checks nothing, which is what a table with no constraints and
// what a table whose schema tail does not parse both get. The second of those
// is deliberate and unchanged from before this file: a tail that cannot be
// read says nothing about what the table declares, and refusing on it would
// newly refuse the writes those tables have always accepted.
type constraintChecks struct {
	notNull []notNullCheck
	bounds  []boundsCheck
}

// notNullCheck is one NOT NULL record resolved to the column it covers.
type notNullCheck struct {
	colIdx int
	column string
	record string
}

// boundsCheck is one CHECK record resolved to the column it covers and to the
// integer bounds it holds. Only the integer family is resolved here; every
// other bound type is refused when the checks are built, so the comparison
// itself has no cases.
type boundsCheck struct {
	colIdx         int
	column         string
	record         string
	min, max       int64
	hasMin, hasMax bool
}

// newConstraintChecks resolves a table's constraint array against its schema,
// refusing with ErrConstraintsNotEnforced any record whose rule this package
// does not test. The refusal is returned rather than recorded, because a
// partially checked table is worse than an unwritable one: it would accept
// writes under a constraint nobody looked at.
func newConstraintChecks(
	constraints []constraintRecord, schema *TableSchema, table string, indexes []maintainedIndex,
) (constraintChecks, error) {
	var checks constraintChecks

	for _, rec := range constraints {
		switch rec.kind {
		case constraintNotNull:
			colIdx, err := constraintColumnIndex(rec, schema)
			if err != nil {
				return constraintChecks{}, fmt.Errorf("%w: table %q: %w", ErrConstraintsNotEnforced, table, err)
			}

			checks.notNull = append(checks.notNull, notNullCheck{
				colIdx: colIdx, column: schema.Columns[colIdx].Name, record: rec.name,
			})
		case constraintCheck:
			colIdx, err := constraintColumnIndex(rec, schema)
			if err != nil {
				return constraintChecks{}, fmt.Errorf("%w: table %q: %w", ErrConstraintsNotEnforced, table, err)
			}

			bounds, err := newBoundsCheck(rec, schema.Columns[colIdx], colIdx)
			if err != nil {
				return constraintChecks{}, fmt.Errorf("%w: table %q: %w", ErrConstraintsNotEnforced, table, err)
			}

			checks.bounds = append(checks.bounds, bounds)
		case constraintPrimaryKey, constraintUnique:
			if err := keyIndexEnforces(rec, indexes, schema); err != nil {
				return constraintChecks{}, fmt.Errorf("%w: table %q: %w", ErrConstraintsNotEnforced, table, err)
			}
		default:
			return constraintChecks{}, fmt.Errorf("%w: table %q declares the constraint %q of kind %d",
				ErrConstraintsNotEnforced, table, rec.name, byte(rec.kind))
		}
	}

	return checks, nil
}

// keyIndexEnforces reports whether the index a key constraint is built on is
// one this writer maintains and refuses duplicates on. It is the whole of what
// checking a key means here: nothing is recorded for the row check, because
// the index does the refusing.
//
// The index is found by object id, which is what the record's ownerObjectID
// holds for kinds 0 and 2 -- the covered column for the other two. Matching on
// the name instead would accept a record naming an index of the same name on
// another table.
func keyIndexEnforces(rec constraintRecord, indexes []maintainedIndex, schema *TableSchema) error {
	for _, idx := range indexes {
		if idx.objectID != rec.ownerID {
			continue
		}

		if !idx.unique {
			return fmt.Errorf("the %s constraint %q is built on index %q, which is not flagged UNIQUE or PRIMARY",
				rec.kind, rec.name, idx.name)
		}

		if len(idx.colIdxs) != len(rec.columns) {
			return fmt.Errorf("the %s constraint %q covers %d columns, but index %q covers %d",
				rec.kind, rec.name, len(rec.columns), idx.name, len(idx.colIdxs))
		}

		for i, covered := range rec.columns {
			colIdx, err := findColumnIndex(schema, covered.name)
			if err != nil {
				return fmt.Errorf("the %s constraint %q: %w", rec.kind, rec.name, err)
			}

			if idx.colIdxs[i] < 0 || idx.colIdxs[i] >= len(schema.Columns) {
				return fmt.Errorf("the %s constraint %q index %q column %d resolves outside the schema",
					rec.kind, rec.name, idx.name, i)
			}

			if idx.colIdxs[i] != colIdx {
				return fmt.Errorf("the %s constraint %q column %d is %q, but index %q covers %q there",
					rec.kind, rec.name, i, covered.name, idx.name, schema.Columns[idx.colIdxs[i]].Name)
			}
		}

		return nil
	}

	return fmt.Errorf("the %s constraint %q names index object %d, which is not an index this package maintains",
		rec.kind, rec.name, rec.ownerID)
}

// constraintColumnIndex resolves the single column a column-shaped record
// covers. A key record names one column or several and is refused by the
// caller, so this only has to insist that a record naming exactly one column
// names a real one.
func constraintColumnIndex(rec constraintRecord, schema *TableSchema) (int, error) {
	if len(rec.columns) != 1 {
		return 0, fmt.Errorf("constraint %q covers %d columns", rec.name, len(rec.columns))
	}

	colIdx, err := findColumnIndex(schema, rec.columns[0].name)
	if err != nil {
		return 0, fmt.Errorf("constraint %q: %w", rec.name, err)
	}

	return colIdx, nil
}

// newBoundsCheck decodes a CHECK record's two bounds against the column they
// constrain. Both have to be stored the way the column itself is -- the same
// base type, and the same width in bytes -- or the comparison would be reading
// a bound the engine wrote for something else. Only the integer family is
// accepted: it is the only one Constraints.abs's CMinMax pins, and a bound of
// another type would need this package to decide what "outside" means for it.
func newBoundsCheck(rec constraintRecord, col Column, colIdx int) (boundsCheck, error) {
	width, signed, ok := integerStorage(col)
	if !ok {
		return boundsCheck{}, fmt.Errorf("constraint %q bounds column %q, which is base type %d",
			rec.name, col.Name, col.BaseType)
	}

	out := boundsCheck{colIdx: colIdx, column: col.Name, record: rec.name}

	for _, b := range []struct {
		v     typedValue
		which string
		out   *int64
		has   *bool
	}{
		{rec.minValue, "MINVALUE", &out.min, &out.hasMin},
		{rec.maxValue, "MAXVALUE", &out.max, &out.hasMax},
	} {
		if !b.v.present {
			continue
		}

		if b.v.baseType != col.BaseType || len(b.v.data) != width {
			return boundsCheck{}, fmt.Errorf(
				"constraint %q holds a %d-byte %s of base type %d for column %q of base type %d",
				rec.name, len(b.v.data), b.which, b.v.baseType, col.Name, col.BaseType,
			)
		}

		*b.out, *b.has = decodeIntegerLE(b.v.data, signed), true
	}

	return out, nil
}

// empty reports whether these checks would test nothing, so a writer can skip
// building a Record over the bytes it is about to store.
func (c constraintChecks) empty() bool {
	return len(c.notNull) == 0 && len(c.bounds) == 0
}

// check tests one encoded record against every constraint resolved for the
// table, naming the record that stopped the write so the caller learns which
// clause it broke rather than that "a constraint" did.
func (c constraintChecks) check(rec Record, table string) error {
	for _, n := range c.notNull {
		if rec.IsNull(n.colIdx) {
			return fmt.Errorf("%w: %s.%s, declared by %q", ErrNotNullViolated, table, n.column, n.record)
		}
	}

	for _, b := range c.bounds {
		// A NULL is not compared: see the file comment on the two rules this
		// package assumes.
		if rec.IsNull(b.colIdx) {
			continue
		}

		v := rec.Int64(b.colIdx)

		if b.hasMin && v < b.min {
			return fmt.Errorf("%w: %s.%s = %d is below MINVALUE %d, declared by %q",
				ErrCheckViolated, table, b.column, v, b.min, b.record)
		}

		if b.hasMax && v > b.max {
			return fmt.Errorf("%w: %s.%s = %d is above MAXVALUE %d, declared by %q",
				ErrCheckViolated, table, b.column, v, b.max, b.record)
		}
	}

	return nil
}

// checkConstraints tests a record this writer is about to store against the
// table's constraint array. It runs after maintainedIndexes, which is what
// resolves the checks, and before any page is touched, so a refused write
// leaves nothing behind.
func (w *TableWriter) checkConstraints(rec []byte) error {
	if w.checks.empty() {
		return nil
	}

	over, err := w.recordOver(rec)
	if err != nil {
		return err
	}

	return w.checks.check(over, w.r.table.Name())
}

// recordOver reads an encoded record the way a stored one is read, so that a
// check can run against bytes that have not been written yet. Every refusal in
// this package happens before a page is touched, and this is what the ones
// that need field values are built on.
func (w *TableWriter) recordOver(rec []byte) (Record, error) {
	n := w.r.nullFlagBytes
	if len(rec) < n+w.r.fieldDataSize {
		return Record{}, fmt.Errorf("%w: %d-byte record, want %d", ErrBadLayout, len(rec), n+w.r.fieldDataSize)
	}

	return Record{
		reader:    w.r,
		nullFlags: rec[:n],
		fieldData: rec[n : n+w.r.fieldDataSize],
	}, nil
}
