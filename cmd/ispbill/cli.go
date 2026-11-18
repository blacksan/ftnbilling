package main

import (
	"fmt"
	"io"
	"strings"
)

const usage = `ispbill: internet billing server

  ispbill                 run the server
  ispbill migrate         apply pending database migrations
  ispbill import-json     [-file PATH] [-force]  load a JSON database into Postgres
  ispbill export-json     -out PATH              write the Postgres data as a JSON file
  ispbill admin-reset-password [-generate]  remove the stored admin password (ADMIN_PASS works again), or set a random one
  ispbill settings-check  show where each setting comes from; fails on unreadable stored values 
  ispbill db-check        [-json] [-against URL] row counts and payment totals

Database commands read DATABASE_URL (and DATA_DIR) from the environment; -url overrides DATABASE_URL.`

// runCLI runs a subcommand. handled is false when there is none (run the server).
func runCLI(args []string, getenv func(string) string, out io.Writer) (handled bool, err error) {
	if len(args) == 0 {
		return false, nil
	}
	switch cmd := args[0]; cmd {
	case "migrate", "import-json", "export-json", "db-check":
		return true, runDBCommand(cmd, args[1:], getenv, out)
	case "admin-reset-password", "settings-check":
		return true, runSettingsCommand(cmd, args[1:], getenv, out)
	case "help", "-h", "--help":
		fmt.Fprintln(out, usage)
		return true, nil
	default:
		return true, fmt.Errorf("unknown command %q\n\n%s", strings.TrimSpace(args[0]), usage)
	}
}
