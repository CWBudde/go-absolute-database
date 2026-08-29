// Command absdb inspects and dumps ComponentAce Absolute Database (.abs) files.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	absdb "github.com/cwbudde/go-absolute-database"
	"github.com/spf13/cobra"
)

// password holds the value of the persistent --password flag. It is shared by
// every subcommand through openDatabase.
var password string

// table holds the value of the persistent --table flag. It is shared by every
// subcommand that reads one table, through selectTable. Empty means the
// database's only table.
var table string

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
	root.PersistentFlags().StringVarP(&table, "table", "t", "",
		"Table to read (default: the only table)")

	root.AddCommand(infoCmd())
	root.AddCommand(tablesCmd())
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

// selectTable resolves the persistent --table flag against the database. When
// the flag is empty and the file holds more than one table, the error names
// them, because "there is more than one" on its own leaves the user guessing.
func selectTable(db *absdb.File) (*absdb.Table, error) {
	t, err := db.Table(table)
	if err == nil {
		return t, nil
	}

	if !errors.Is(err, absdb.ErrAmbiguousTable) {
		return nil, err
	}

	tables, listErr := db.Tables()
	if listErr != nil {
		return nil, err
	}

	names := make([]string, 0, len(tables))
	for _, info := range tables {
		names = append(names, info.Name)
	}

	return nil, fmt.Errorf("%w: pass --table with one of %s", err, strings.Join(names, ", "))
}

// yes is what every column of every table this command prints uses to mark a
// boolean that is set; an unset one is left blank so the set ones stand out.
const yes = "yes"
