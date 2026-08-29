package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// An AUTOINC column's next value is not derived from its rows. It is stored,
// one int64 per column, in the array the table info file carries between its
// column count and its two trailing counters -- the array buildTableInfoFile
// writes as zeroes and whose slots this file calls counters.
//
// That array was read as padding for a long time, because in a table with no
// AUTOINC column every slot of it is zero, and the two Auto*.abs fixtures that
// existed first were read as saying no counter is stored at all: an AUTOINC
// insert touches the same page *types* an Int32-keyed insert touches. It does,
// and the counter is on one of them -- the table info page an insert rewrites
// anyway for the record count.
//
// Seven fixtures pin what moves it, and the rule is narrower than "the column's
// maximum":
//
//   - An insert raises the counter to the value inserted when that value is
//     larger (Auto-insexp.abs: 3 -> 10) and leaves it alone when it is smaller
//     (Auto-inslow.abs: still 10 after inserting 5).
//   - An insert that supplies no value takes counter+increment
//     (Auto-insnext.abs: 11 rather than 4, Auto-delins.abs: 4 rather than the
//     3 a delete had freed).
//   - A delete never lowers it (Auto-del.abs) and an update never raises it
//     (Auto-upd.abs sets Id to 20 and leaves the counter at 3). So the counter
//     records what has been *assigned*, not what the column holds, and a writer
//     that recomputed it from the rows would be wrong on exactly that file.
//   - Compaction rebuilds it from the rows (Auto-updcompact.abs: 3 -> 20), which
//     is not a further rule but this one applied by a replay. This package gets
//     it for the same reason the engine does: copyTableRows re-inserts every row
//     through Insert.
//
// Across the corpus all twenty AUTOINC columns carry their column's maximum,
// and no other column anywhere carries a non-zero counter. Types.abs's TAutoInc
// is what separates the counter from a row count: INITIALVALUE 100 INCREMENT 5
// gives it two rows and a counter of 110.

var (
	// ErrAutoIncNotMaintained reports an AUTOINC column whose counter this
	// package will not maintain, which refuses every write to its table rather
	// than leaving the counter stale. A stale counter is worse than a refusal:
	// the engine's next insert would reissue a value the table already holds,
	// and on a PRIMARY KEY column -- which is what fifteen of the corpus's
	// twenty-five key constraints are -- the engine would then refuse its own
	// write.
	ErrAutoIncNotMaintained = errors.New("absdb: AUTOINC column this package cannot maintain")

	// ErrAutoIncExhausted reports an assignment past the column's declared
	// MAXVALUE. Whether the engine refuses it or wraps is not known -- no
	// fixture reaches a bound -- so this package stops rather than writing a
	// value the column's own declaration forbids.
	ErrAutoIncExhausted = errors.New("absdb: AUTOINC column has reached its MAXVALUE")
)

// autoIncCounter is one AUTOINC column's counter: where it lives, what it
// holds, and the column's declared parameters.
type autoIncCounter struct {
	col       int   // the column's index in the schema
	off       int   // its counter's offset within the table info page payload
	value     int64 // the counter as it now stands, assignments included
	increment int64
	maxValue  int64
	raised    bool
}

// autoIncCounters resolves the table's AUTOINC counters, reading the table info
// page once and memoising the result on the writer.
//
// A table with no AUTOINC column resolves to an empty slice and costs one page
// read, which every write already performs for the record count.
func (w *TableWriter) autoIncCounters() ([]autoIncCounter, error) {
	if w.autoIncResolved {
		return w.autoInc, w.autoIncErr
	}

	w.autoIncResolved = true
	w.autoInc, w.autoIncErr = w.readAutoIncCounters()

	return w.autoInc, w.autoIncErr
}

// readAutoIncCounters locates and reads one counter per AUTOINC column.
func (w *TableWriter) readAutoIncCounters() ([]autoIncCounter, error) {
	columns := w.r.Schema().Columns

	wanted := 0

	for _, col := range columns {
		if col.IsAutoInc() {
			wanted++
		}
	}

	if wanted == 0 {
		return nil, nil
	}

	no, err := w.r.table.infoPageNo()
	if err != nil {
		return nil, err
	}

	if no < 0 {
		return nil, fmt.Errorf("%w: table %q has no table info page", ErrAutoIncNotMaintained, w.r.table.Name())
	}

	buf, err := w.loadPage(no)
	if err != nil {
		return nil, err
	}

	base, err := autoIncCounterBase(buf.payload, len(columns))
	if err != nil {
		return nil, err
	}

	out := make([]autoIncCounter, 0, wanted)

	for i, col := range columns {
		if !col.IsAutoInc() {
			continue
		}

		if col.FieldType != FieldAutoInc || col.BaseType != BftInt32 {
			return nil, fmt.Errorf("%w: column %q is base type %d / field type %s",
				ErrAutoIncNotMaintained, col.Name, col.BaseType, col.FieldType)
		}

		opts := col.autoInc
		if opts.cycled {
			return nil, fmt.Errorf("%w: column %q is CYCLED, which no fixture shows wrapping",
				ErrAutoIncNotMaintained, col.Name)
		}

		increment, maxValue := int64(1), int64(math.MaxInt64)
		if opts.known {
			increment, maxValue = opts.increment, opts.maxValue
		}

		if increment <= 0 {
			return nil, fmt.Errorf("%w: column %q increments by %d",
				ErrAutoIncNotMaintained, col.Name, increment)
		}

		off := base + i*tableInfoCounterFields

		out = append(out, autoIncCounter{
			col:       i,
			off:       off,
			value:     int64(binary.LittleEndian.Uint64(buf.payload[off : off+8])),
			increment: increment,
			maxValue:  maxValue,
		})
	}

	return out, nil
}

// autoIncCounterBase locates the per-column counter array inside a table info
// page's payload, checking that it describes the table being written.
//
// The array's own column count is checked against the schema's rather than
// trusted: the counters are addressed by column index, so a file whose two
// disagree would have this package raise the wrong column's counter.
func autoIncCounterBase(payload []byte, columns int) (int, error) {
	if len(payload) < internalFileHeaderSize {
		return 0, fmt.Errorf("%w: table info page is %d bytes", ErrBookkeepingMismatch, len(payload))
	}

	hdrSize := int(payload[0])
	stored := int(binary.LittleEndian.Uint32(payload[1:5]))

	want := 4 + columns*tableInfoCounterFields + tableInfoTrailerSize
	if hdrSize < internalFileHeaderSize || stored != want || hdrSize+stored > len(payload) {
		return 0, fmt.Errorf("%w: table info file declares %d bytes behind a %d-byte header, want %d for %d columns",
			ErrBookkeepingMismatch, stored, hdrSize, want, columns)
	}

	declared := int(binary.LittleEndian.Uint32(payload[hdrSize : hdrSize+4]))
	if declared != columns {
		return 0, fmt.Errorf("%w: table info file is for %d columns, the schema has %d",
			ErrBookkeepingMismatch, declared, columns)
	}

	return hdrSize + 4, nil
}

// assignAutoInc fills in a nil value for an AUTOINC column with the counter's
// next value, returning the values to encode.
//
// A nil means "let the column number this row", which is what omitting it from
// an INSERT does in the engine's own SQL and how Auto.abs's three rows and
// Auto-ins.abs's fourth were written. It does not mean NULL: no AUTOINC column
// in the corpus holds one, and on the PRIMARY KEY column such a column usually
// is, the engine refuses a NULL outright.
//
// The input slice is not modified; a copy is made only when there is something
// to fill in.
func (w *TableWriter) assignAutoInc(values []any) ([]any, error) {
	counters, err := w.autoIncCounters()
	if err != nil {
		return nil, err
	}

	if len(values) != len(w.r.Schema().Columns) {
		// encodeRecord reports the count mismatch; assigning into a slice of
		// the wrong length would only obscure it.
		return values, nil
	}

	out, copied := values, false

	for i := range counters {
		c := &counters[i]

		if values[c.col] != nil {
			continue
		}

		next := c.value + c.increment
		if next < c.value || next > c.maxValue {
			return nil, fmt.Errorf("%w: column %q at %d, increment %d, MAXVALUE %d",
				ErrAutoIncExhausted, w.r.Schema().Columns[c.col].Name, c.value, c.increment, c.maxValue)
		}

		if next > math.MaxInt32 {
			return nil, fmt.Errorf("%w: column %q would take %d, past what an Int32 column holds",
				ErrAutoIncExhausted, w.r.Schema().Columns[c.col].Name, next)
		}

		if !copied {
			out, copied = make([]any, len(values)), true
			copy(out, values)
		}

		out[c.col] = int32(next) //nolint:gosec // bounded against math.MaxInt32 just above
	}

	return out, nil
}

// raiseAutoInc advances each AUTOINC counter to the value the record about to
// be inserted carries, when that value is larger.
//
// Only an insert calls it. An update never raises the counter and a delete
// never lowers it, which Auto-upd.abs and Auto-del.abs settle: the counter is
// what the engine has handed out, not what the column holds.
func (w *TableWriter) raiseAutoInc(rec []byte) error {
	counters, err := w.autoIncCounters()
	if err != nil {
		return err
	}

	if len(counters) == 0 {
		return nil
	}

	over, err := w.recordOver(rec)
	if err != nil {
		return err
	}

	for i := range counters {
		c := &counters[i]

		if over.IsNull(c.col) {
			continue
		}

		if v := int64(over.Int(c.col)); v > c.value {
			c.value, c.raised = v, true
		}
	}

	return nil
}

// writeAutoIncCounters writes back every counter this transaction moved. It is
// called from updateTableInfo, so the counters land on the same page as the
// record and change counts and in the same transaction.
func (w *TableWriter) writeAutoIncCounters() error {
	moved := false

	for _, c := range w.autoInc {
		if c.raised {
			moved = true

			break
		}
	}

	if !moved {
		return nil
	}

	no, err := w.r.table.infoPageNo()
	if err != nil {
		return err
	}

	buf, err := w.loadPage(no)
	if err != nil {
		return err
	}

	for _, c := range w.autoInc {
		if !c.raised {
			continue
		}

		binary.LittleEndian.PutUint64(buf.payload[c.off:c.off+8], uint64(c.value))
	}

	buf.dirty = true

	return nil
}
