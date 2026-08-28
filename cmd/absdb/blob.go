package main

import (
	"fmt"
	"os"
	"strconv"

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

			db, err := openDatabase(args[0])
			if err != nil {
				return err
			}
			defer db.Close()

			reader, err := db.OpenTable()
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

			// Skip to the requested row.
			i := 0

			for reader.Next() {
				if i == row {
					rec := reader.Record()

					data, err := rec.Blob(col)
					if err != nil {
						return err
					}

					if data == nil {
						fmt.Fprintf(os.Stderr, "BLOB at row %d, col %d (%s) is NULL\n",
							row, col, schema.Columns[col].Name)

						return nil
					}

					if output != "" {
						err = os.WriteFile(output, data, 0o644)
						if err != nil {
							return err
						}

						fmt.Fprintf(os.Stderr, "Wrote %d bytes to %s\n", len(data), output)
					} else {
						_, err = os.Stdout.Write(data)
					}

					return err
				}

				reader.Record() // consume to advance
				i++
			}

			if err := reader.Err(); err != nil {
				return err
			}

			return fmt.Errorf("row %d not found (only %d rows)", row, i)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Write BLOB to file instead of stdout")

	return cmd
}
