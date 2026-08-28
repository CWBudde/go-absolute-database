// Command absdb inspects and dumps ComponentAce Absolute Database (.abs) files.
package main

import (
	"fmt"
	"os"

	absdb "github.com/cwbudde/go-absolute-database"
	"github.com/spf13/cobra"
)

// password holds the value of the persistent --password flag. It is shared by
// every subcommand through openDatabase.
var password string

func main() {
	root := &cobra.Command{
		Use:   "absdb",
		Short: "Inspect Absolute Database (.abs) files",
		// Runtime failures are not usage errors; printing the full usage
		// block after every one of them just buries the message.
		SilenceUsage: true,
	}

	root.PersistentFlags().StringVarP(&password, "password", "p", "",
		"Password for encrypted databases")

	root.AddCommand(infoCmd())
	root.AddCommand(pagesCmd())
	root.AddCommand(schemaCmd())
	root.AddCommand(dumpCmd())
	root.AddCommand(hexpageCmd())
	root.AddCommand(blobCmd())

	err := root.Execute()
	if err != nil {
		os.Exit(1)
	}
}

// openDatabase opens path and, if the file is encrypted, unlocks it with the
// password from the persistent --password flag.
//
// An encrypted file opened without a password would silently yield ciphertext,
// so this reports the problem instead.
func openDatabase(path string) (*absdb.File, error) {
	db, err := absdb.Open(path)
	if err != nil {
		return nil, err
	}

	if !db.Encrypted() {
		return db, nil
	}

	if password == "" {
		algo := "unknown algorithm"
		if ch := db.CryptoHeader(); ch != nil {
			algo = ch.Algorithm.String()
		}

		db.Close()

		return nil, fmt.Errorf("%s is encrypted (%s): supply the password with --password", path, algo)
	}

	err = db.Unlock(password)
	if err != nil {
		db.Close()

		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return db, nil
}
