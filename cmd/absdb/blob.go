package main

import (
	"fmt"
	"os"
	"strconv"

	absdb "github.com/cwbudde/go-absolute-database"
	"github.com/spf13/cobra"
)

func blobCmd() *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "blob <file> <row> <col>",
		Short: "Extract a BLOB value (row and col are 0-based)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			row, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("invalid row: %s", args[1])
			}

			col, err := strconv.Atoi(args[2])
			if err != nil {
				return fmt.Errorf("invalid col: %s", args[2])
			}

			return extractBlob(args[0], row, col, output)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Write BLOB to file instead of stdout")

	return cmd
}

// extractBlob reads the BLOB stored at the given 0-based row and column of the
// database's table and writes it to output, or to stdout when output is empty.
func extractBlob(path string, row, col int, output string) error {
	db, err := openDatabase(path)
	if err != nil {
		return err
	}
	defer db.Close()

	tbl, err := selectTable(db)
	if err != nil {
		return err
	}

	reader, err := tbl.Open()
	if err != nil {
		return err
	}

	schema := reader.Schema()
	if col < 0 || col >= len(schema.Columns) {
		return fmt.Errorf("column %d out of range (0-%d)", col, len(schema.Columns)-1)
	}

	if !schema.Columns[col].IsBLOB() {
		return fmt.Errorf("column %d (%s) is not a BLOB column",
			col, schema.Columns[col].Name)
	}

	rec, err := seekRow(reader, row)
	if err != nil {
		return err
	}

	data, err := rec.Blob(col)
	if err != nil {
		return err
	}

	if data == nil {
		fmt.Fprintf(os.Stderr, "BLOB at row %d, col %d (%s) is NULL\n",
			row, col, schema.Columns[col].Name)

		return nil
	}

	return writeBlob(data, output)
}

// seekRow advances reader to the given 0-based row and returns its record. It
// reports an error when the reader fails or when the table has fewer rows.
func seekRow(reader *absdb.Reader, row int) (absdb.Record, error) {
	count := 0

	for reader.Next() {
		// Record is called on every row, not only the wanted one: it is what
		// consumes the current record and advances the reader.
		rec := reader.Record()
		if count == row {
			return rec, nil
		}

		count++
	}

	if err := reader.Err(); err != nil {
		return absdb.Record{}, err
	}

	return absdb.Record{}, fmt.Errorf("row %d not found (only %d rows)", row, count)
}

// writeBlob writes data to the named file, or to stdout when output is empty.
func writeBlob(data []byte, output string) error {
	if output == "" {
		_, err := os.Stdout.Write(data)

		return err
	}

	// 0o600, not 0o644: an extracted BLOB is verbatim payload out of a private
	// database, so the file it lands in is readable by its owner only.
	if err := os.WriteFile(output, data, 0o600); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Wrote %d bytes to %s\n", len(data), output)

	return nil
}
