// Command absdb inspects and dumps ComponentAce Absolute Database (.abs) files.
package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "absdb",
		Short: "Inspect Absolute Database (.abs) files",
	}

	root.AddCommand(infoCmd())
	root.AddCommand(pagesCmd())
	root.AddCommand(schemaCmd())
	root.AddCommand(dumpCmd())
	root.AddCommand(hexpageCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
