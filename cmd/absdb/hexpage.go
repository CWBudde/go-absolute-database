package main

import (
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

func hexpageCmd() *cobra.Command {
	var raw bool

	cmd := &cobra.Command{
		Use:   "hexpage <file> <page>",
		Short: "Hex dump a specific page",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pageNo, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("invalid page number: %s", args[1])
			}

			db, err := openDatabase(args[0])
			if err != nil {
				return err
			}
			defer db.Close()

			page, err := db.ReadPage(pageNo)
			if err != nil {
				return err
			}

			if page.Header != nil {
				fmt.Printf("Page %d: type=%s, next=%d, objID=%d\n\n",
					pageNo, pageTypeName(page.Header.PageType),
					page.Header.NextPageNo, page.Header.ObjectID)
			}

			var data []byte
			if raw {
				data = page.Data
			} else {
				data = page.PageData()
			}

			if data == nil {
				fmt.Println("(no data)")
				return nil
			}

			fmt.Print(hex.Dump(data))

			return nil
		},
	}

	cmd.Flags().BoolVar(&raw, "raw", false, "Dump full page including header area")

	return cmd
}
