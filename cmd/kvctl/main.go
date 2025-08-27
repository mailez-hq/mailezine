// kvctl manages the engine's TiDB kv table for dev/e2e runs.
//
// The engine always uses the fixed table "mailezine_kv" (see
// internal/app/storage.go). Because the blob root is configurable, a
// half-reused table from an earlier run can point at blobs that no longer
// exist, which surfaces as "store: not found" on IMAP reads. -drop clears
// the table so an e2e run starts from a clean slate.
//
// Usage:
//
//	go run ./cmd/kvctl -dsn root@tcp(127.0.0.1:4000)/test -drop
//	go run ./cmd/kvctl -dsn root@tcp(127.0.0.1:4000)/test -dump
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	dsn := flag.String("dsn", "root@tcp(127.0.0.1:4000)/test", "TiDB/MySQL DSN")
	drop := flag.Bool("drop", false, "drop the engine kv table")
	dump := flag.Bool("dump", false, "print keys of the engine kv table")
	table := flag.String("table", "mailezine_kv", "engine kv table name")
	flag.Parse()

	db, err := sql.Open("mysql", *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		fmt.Fprintln(os.Stderr, "ping:", err)
		os.Exit(1)
	}
	switch {
	case *drop:
		if _, err := db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS `%s`", *table)); err != nil {
			fmt.Fprintln(os.Stderr, "drop:", err)
			os.Exit(1)
		}
		fmt.Printf("dropped table %s\n", *table)
	case *dump:
		rows, err := db.Query(fmt.Sprintf("SELECT k, LENGTH(v) FROM `%s` ORDER BY k", *table))
		if err != nil {
			fmt.Fprintln(os.Stderr, "dump:", err)
			os.Exit(1)
		}
		defer rows.Close()
		for rows.Next() {
			var k []byte
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				fmt.Fprintln(os.Stderr, "scan:", err)
				os.Exit(1)
			}
			printable := ""
			for _, b := range k {
				if b >= 32 && b < 127 {
					printable += string(rune(b))
				} else {
					printable += fmt.Sprintf("\\x%02x", b)
				}
			}
			fmt.Printf("len=%d %s\n", n, printable)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: kvctl -drop | -dump")
		os.Exit(2)
	}
}
