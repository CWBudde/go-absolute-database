package main

import (
	"fmt"

	absdb "github.com/meko-tech/go-absolute-database"
	"github.com/spf13/cobra"
)

func infoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <file>",
		Short: "Show file header information",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := absdb.Open(args[0])
			if err != nil {
				return err
			}
			defer db.Close()

			fmt.Printf("File:       %s\n", args[0])
			fmt.Printf("Version:    %.2f\n", db.Version())
			fmt.Printf("Page size:  %d bytes\n", db.PageSize())
			fmt.Printf("Page count: %d\n", db.PageCount())
			fmt.Printf("Encrypted:  %v\n", db.Encrypted())

			schema, err := db.Schema()
			if err != nil {
				fmt.Printf("Schema:     (error: %v)\n", err)
			} else {
				fmt.Printf("Columns:    %d\n", len(schema.Columns))
			}

			return nil
		},
	}
}
