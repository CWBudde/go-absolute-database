package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func infoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <file>",
		Short: "Show file header information",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openDatabase(args[0])
			if err != nil {
				return err
			}
			defer db.Close()

			fmt.Printf("File:       %s\n", args[0])
			fmt.Printf("Version:    %.2f\n", db.Version())
			fmt.Printf("Page size:  %d bytes\n", db.PageSize())
			fmt.Printf("Page count: %d\n", db.PageCount())

			if ch := db.CryptoHeader(); ch != nil {
				fmt.Printf("Encrypted:  yes (%s, mode %d)\n", ch.Algorithm, ch.Mode)
			} else {
				fmt.Printf("Encrypted:  %v\n", db.Encrypted())
			}

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
