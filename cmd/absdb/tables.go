package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func tablesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tables <file>",
		Short: "List the tables in the database",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			db, err := openDatabase(args[0])
			if err != nil {
				return err
			}
			defer db.Close()

			tables, err := db.Tables()
			if err != nil {
				return err
			}

			fmt.Printf("%-4s %-32s %-8s %-8s\n", "ID", "Name", "Schema", "Info")
			fmt.Printf("%-4s %-32s %-8s %-8s\n", "--", "----", "------", "----")

			for _, t := range tables {
				fmt.Printf("%-4d %-32s %-8d %-8d\n", t.ID, t.Name, t.SchemaPageNo, t.InfoPageNo)
			}

			return nil
		},
	}
}
