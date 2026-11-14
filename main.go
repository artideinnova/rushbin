// rushbin catalogs video drives. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const usage = `usage: rushbin scan [-catalog DIR] [-id ID] [-name NAME] DRIVE [EXCLUDED_FOLDER ...]

Writes DIR/disks/ID/{device.json,files.jsonl,fullhash.tsv} and the previews in DIR/thumbs, by default in the same folder the
Mac app uses (~/Library/Application Support/rushbin on macOS). Nothing is written to the drive.
Without excluded folders, the ones from the previous scan of the same drive are reused.
Without -name, the name from the previous scan is kept, otherwise the volume name is used.`

func main() {
	cfg, _ := os.UserConfigDir()                        // ~/Library/Application Support on macOS
	if runtime.GOOS == "windows" && len(os.Args) == 1 { // double-clicked rushbin.exe: the app
		os.Args = append(os.Args, "app")
	}
	if len(os.Args) > 1 && os.Args[1] == "app" { // started by the Mac app (or by a double click on Windows)
		f := flag.NewFlagSet("app", flag.ExitOnError)
		catalog := f.String("catalog", filepath.Join(cfg, "rushbin"), "catalog directory")
		addr := f.String("addr", "127.0.0.1:0", "listen address")
		f.Parse(os.Args[2:])
		log.Fatal(runApp(*catalog, *addr))
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" { // the team site; DATABASE_URL points to Postgres
		f := flag.NewFlagSet("serve", flag.ExitOnError)
		addr := f.String("addr", ":8080", "listen address")
		data := f.String("data", "/var/lib/rushbin", "folder for the previews")
		f.Parse(os.Args[2:])
		log.Fatal(runServer(context.Background(), os.Getenv("DATABASE_URL"), *data, *addr))
	}
	if len(os.Args) > 1 && os.Args[1] == "newteam" { // a new team on the site: prints its owner's invite link
		f := flag.NewFlagSet("newteam", flag.ExitOnError)
		site := f.String("url", os.Getenv("RUSHBIN_URL"), "address of the site, like https://rushbin.example.com")
		f.Parse(os.Args[2:])
		if f.NArg() != 2 || *site == "" {
			fmt.Fprintln(os.Stderr, `usage: rushbin newteam [-url SITE] "Team name" owner@email`)
			os.Exit(2)
		}
		s, err := openServer(context.Background(), os.Getenv("DATABASE_URL"), "")
		if err != nil {
			log.Fatal(err)
		}
		tok, err := newTeam(context.Background(), s, f.Arg(0), f.Arg(1))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Send this link to %s, it works once for %d days:\n%s/login#invite=%s\n", f.Arg(1), linkDays, strings.TrimRight(*site, "/"), tok)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "setup-link" { // the link that creates a new site's first team, on this terminal only
		s, err := openServer(context.Background(), os.Getenv("DATABASE_URL"), "")
		if err != nil {
			log.Fatal(err)
		}
		tok, err := setupLink(context.Background(), s)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Open this link to create the first team. It works once, until a newer link replaces it:\n%s/login#setup=%s\n",
			strings.TrimRight(os.Getenv("RUSHBIN_URL"), "/"), tok)
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "scan" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	f := flag.NewFlagSet("scan", flag.ExitOnError)
	f.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	catalog := f.String("catalog", filepath.Join(cfg, "rushbin"), "catalog directory")
	id := f.String("id", "", "drive ID (default: the volume UUID)")
	name := f.String("name", "", `drive name, for example "LaCie 4TB Milan"`)
	f.Parse(os.Args[2:])
	if f.NArg() < 1 {
		f.Usage()
		os.Exit(2)
	}
	drive, err := scan(*catalog, f.Arg(0), *id, *name, f.Args()[1:])
	if err == nil {
		err = checkDuplicates(*catalog, drive, filepath.Base(f.Arg(0)))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rushbin:", err)
		os.Exit(1)
	}
	a := &App{dir: *catalog} // the previews, as the app makes them: a folder that is not a volume gets them too
	if err := a.reload(); err != nil {
		fmt.Fprintln(os.Stderr, "rushbin:", err)
		os.Exit(1)
	}
	a.thumbs(drive)
}
