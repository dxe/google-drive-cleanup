// Command google-drive-cleanup crawls a Google Drive folder tree into a
// SQLite database and reports on file ownership. It is the first half of an
// ownership-migration project: we snapshot every file's location and owner
// before moving files through shared drives for ownership transfer, so they
// can be moved back to their original parents afterwards. See README.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "crawl":
		err = cmdCrawl(os.Args[2:])
	case "owners":
		err = cmdOwners(os.Args[2:])
	case "path":
		err = cmdPath(os.Args[2:])
	case "help", "-h", "-help", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: google-drive-cleanup <subcommand> [flags]

Subcommands:
  crawl   recursively crawl the configured root folder into the database
          flags: -db drive.db  -root-config root.json  -refresh
  owners  print each owner and how many files (non-folders) they own,
          sorted descending by count
          flags: -db drive.db
  path    print the full folder path of a node by Drive ID
          usage: path [-db drive.db] <drive_id>
`)
}

func cmdOwners(args []string) error {
	fs := flag.NewFlagSet("owners", flag.ExitOnError)
	dbPath := fs.String("db", "drive.db", "path to the SQLite database")
	fs.Parse(args)

	db, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	counts, err := ownersReport(db)
	if err != nil {
		return err
	}
	for _, oc := range counts {
		fmt.Printf("%8d  %s\n", oc.count, ownerLabel(oc))
	}
	return nil
}

func ownerLabel(oc ownerCount) string {
	switch {
	case oc.email.Valid:
		if oc.displayName.Valid {
			return fmt.Sprintf("%s (%s)", oc.email.String, oc.displayName.String)
		}
		return oc.email.String
	case oc.ownerID.Valid:
		// No email on these rows; the stable Drive user id is all we have, so
		// include the display name to make it human-readable.
		if oc.displayName.Valid {
			return fmt.Sprintf("id:%s (%s)", oc.ownerID.String, oc.displayName.String)
		}
		return "id:" + oc.ownerID.String
	default:
		return "(unknown)"
	}
}

func cmdPath(args []string) error {
	fs := flag.NewFlagSet("path", flag.ExitOnError)
	dbPath := fs.String("db", "drive.db", "path to the SQLite database")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: path [-db drive.db] <drive_id>")
	}

	db, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	segments, err := nodePath(db, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Println(strings.Join(segments, " / "))
	return nil
}
