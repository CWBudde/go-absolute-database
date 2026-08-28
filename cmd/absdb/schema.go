package main

import (
	"fmt"

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

			schema, err := db.Schema()
			if err != nil {
				return err
			}

			fmt.Printf("%-4s %-25s %-15s %-15s %-6s %-6s\n",
				"#", "Name", "FieldType", "BaseType", "Size", "BLOB")
			fmt.Printf("%-4s %-25s %-15s %-15s %-6s %-6s\n",
				"--", "----", "---------", "--------", "----", "----")

			for i, c := range schema.Columns {
				blob := ""
				if c.IsBLOB() {
					blob = "yes"
				}

				fmt.Printf("%-4d %-25s %-15s %-15d %-6d %-6s\n",
					i, c.Name, c.FieldType.String(), c.BaseType, c.Size, blob)
			}

			return nil
		},
	}
}
