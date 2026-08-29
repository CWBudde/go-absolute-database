package main

import (
	"fmt"

	absdb "github.com/cwbudde/go-absolute-database"
	"github.com/spf13/cobra"
)

func schemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema <file>",
		Short: "Show table column definitions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openDatabase(args[0])
			if err != nil {
				return err
			}
			defer db.Close()

			tbl, err := selectTable(db)
			if err != nil {
				return err
			}

			schema, err := tbl.Schema()
			if err != nil {
				return err
			}

			fmt.Printf("%-4s %-25s %-15s %-15s %-6s %-6s %-9s %-8s\n",
				"#", "Name", "FieldType", "BaseType", "Size", "BLOB", "Null", "Default")
			fmt.Printf("%-4s %-25s %-15s %-15s %-6s %-6s %-9s %-8s\n",
				"--", "----", "---------", "--------", "----", "----", "----", "-------")

			for i, c := range schema.Columns {
				blob := ""
				if c.IsBLOB() {
					blob = yes
				}

				def := ""
				if c.HasDefault() {
					def = yes
				}

				fmt.Printf("%-4d %-25s %-15s %-15d %-6d %-6s %-9s %-8s\n",
					i, c.Name, c.FieldType.String(), c.BaseType, c.Size, blob,
					nullability(c), def)
			}

			return nil
		},
	}
}

// nullability renders a column's NOT NULL state for the table above. The third
// case is not padding: Column.NotNull reports separately that nothing was
// established, which happens when the table's constraint array does not parse,
// and printing that as "NULL" would claim more than the file said.
func nullability(c absdb.Column) string {
	notNull, known := c.NotNull()

	switch {
	case !known:
		return "?"
	case notNull:
		return "NOT NULL"
	default:
		return "NULL"
	}
}
