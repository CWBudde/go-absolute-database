package main

import (
	"fmt"

	absdb "github.com/meko-tech/go-absolute-database"
	"github.com/spf13/cobra"
)

func pageTypeName(pt uint16) string {
	switch pt {
	case absdb.PageTypeSystemDir:
		return "SystemDir"
	case absdb.PageTypeFileHdr:
		return "FileHdr"
	case absdb.PageTypeSchema:
		return "Schema"
	case absdb.PageTypeData:
		return "Data"
	case absdb.PageTypeIndex:
		return "Index"
	default:
		return fmt.Sprintf("Type_%d", pt)
	}
}

func pagesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pages <file>",
		Short: "List all pages with type and chain info",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := absdb.Open(args[0])
			if err != nil {
				return err
			}
			defer db.Close()

			fmt.Printf("%-6s %-12s %-6s %-6s %-6s\n", "Page", "Type", "Next", "ObjID", "Empty")
			fmt.Printf("%-6s %-12s %-6s %-6s %-6s\n", "----", "----", "----", "-----", "-----")

			for i := range db.PageCount() {
				page, err := db.ReadPage(i)
				if err != nil {
					return err
				}

				typeName := "(none)"
				next := ""
				objID := ""

				if page.Header != nil {
					typeName = pageTypeName(page.Header.PageType)
					if page.Header.NextPageNo >= 0 {
						next = fmt.Sprintf("%d", page.Header.NextPageNo)
					} else {
						next = "-"
					}
					objID = fmt.Sprintf("%d", page.Header.ObjectID)
				}

				empty := ""
				if page.IsEmpty() {
					empty = "yes"
				}

				fmt.Printf("%-6d %-12s %-6s %-6s %-6s\n", i, typeName, next, objID, empty)
			}

			return nil
		},
	}
}
