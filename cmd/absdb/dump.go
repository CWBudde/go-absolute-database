package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	absdb "github.com/cwbudde/go-absolute-database"
	"github.com/spf13/cobra"
)

func dumpCmd() *cobra.Command {
	var jsonOutput bool
	var limit int

	cmd := &cobra.Command{
		Use:   "dump <file>",
		Short: "Dump all records as a table or JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			if jsonOutput {
				return dumpJSON(reader, schema, limit)
			}

			return dumpTable(reader, schema, limit)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	cmd.Flags().IntVarP(&limit, "limit", "n", 0, "Max rows to output (0 = all)")

	return cmd
}

func dumpTable(reader *absdb.Reader, schema *absdb.TableSchema, limit int) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)

	// Header.
	var headers []string
	for _, c := range schema.Columns {
		headers = append(headers, c.Name)
	}

	fmt.Fprintln(w, strings.Join(headers, "\t"))

	count := 0

	for reader.Next() {
		if limit > 0 && count >= limit {
			break
		}

		rec := reader.Record()
		var vals []string

		for i, c := range schema.Columns {
			vals = append(vals, formatField(rec, i, c))
		}

		fmt.Fprintln(w, strings.Join(vals, "\t"))
		count++
	}

	if err := reader.Err(); err != nil {
		return err
	}

	return w.Flush()
}

func dumpJSON(reader *absdb.Reader, schema *absdb.TableSchema, limit int) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	count := 0

	fmt.Println("[")

	first := true

	for reader.Next() {
		if limit > 0 && count >= limit {
			break
		}

		rec := reader.Record()
		row := make(map[string]any)

		for i, c := range schema.Columns {
			if rec.IsNull(i) {
				row[c.Name] = nil
				continue
			}

			row[c.Name] = fieldValue(rec, i, c)
		}

		if !first {
			fmt.Print(",\n")
		}

		first = false

		if err := enc.Encode(row); err != nil {
			return err
		}

		count++
	}

	fmt.Println("]")

	return reader.Err()
}

func formatField(rec absdb.Record, col int, c absdb.Column) string {
	if rec.IsNull(col) {
		return "<null>"
	}

	switch c.BaseType {
	case absdb.BftInt8, absdb.BftInt16, absdb.BftInt32:
		return fmt.Sprintf("%d", rec.Int(col))
	case absdb.BftInt64:
		return fmt.Sprintf("%d", rec.Int64(col))
	case absdb.BftUint8, absdb.BftUint16, absdb.BftUint32:
		return fmt.Sprintf("%d", rec.Uint32(col))
	case absdb.BftSingle, absdb.BftDouble, absdb.BftCurrency:
		return fmt.Sprintf("%g", rec.Float(col))
	case absdb.BftLogical:
		return fmt.Sprintf("%v", rec.Bool(col))
	case absdb.BftDate, absdb.BftTime, absdb.BftDateTime:
		return rec.Time(col).Format("2006-01-02 15:04:05")
	case absdb.BftVarchar, absdb.BftChar, absdb.BftWideVarchar, absdb.BftWideChar:
		return rec.String(col)
	case absdb.BftBlob, absdb.BftClob, absdb.BftWideClob:
		ref := rec.BlobRef(col)
		if ref.IsNull() {
			return "<blob:null>"
		}

		return fmt.Sprintf("<blob:page %d, item %d>", ref.PageNo, ref.ItemNo)
	default:
		return fmt.Sprintf("<%d bytes>", c.Size)
	}
}

func fieldValue(rec absdb.Record, col int, c absdb.Column) any {
	switch c.BaseType {
	case absdb.BftInt8, absdb.BftInt16, absdb.BftInt32:
		return rec.Int(col)
	case absdb.BftInt64:
		return rec.Int64(col)
	case absdb.BftUint8, absdb.BftUint16, absdb.BftUint32:
		return rec.Uint32(col)
	case absdb.BftSingle, absdb.BftDouble, absdb.BftCurrency:
		return rec.Float(col)
	case absdb.BftLogical:
		return rec.Bool(col)
	case absdb.BftDate, absdb.BftTime, absdb.BftDateTime:
		return rec.Time(col).Format("2006-01-02T15:04:05")
	case absdb.BftVarchar, absdb.BftChar, absdb.BftWideVarchar, absdb.BftWideChar:
		return rec.String(col)
	case absdb.BftBlob, absdb.BftClob, absdb.BftWideClob:
		data, err := rec.Blob(col)
		if err != nil || data == nil {
			return nil
		}

		// For Memo/Clob, return as string; for binary BLOBs, return size info.
		if c.BaseType == absdb.BftClob || c.BaseType == absdb.BftWideClob {
			return string(data)
		}

		return fmt.Sprintf("<blob:%d bytes>", len(data))
	default:
		return fmt.Sprintf("<%d bytes>", c.Size)
	}
}
