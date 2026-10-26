package main

import (
	"context"
	"database/sql"
	"log"
)

// failOnCopyWarnings stops the tool on any warning from the copy statement
// just run on conn. The copy keeps a row the triggers wrote first with a no-op
// on duplicate key update, which raises no warning, so a warning here is a row
// that broke a check constraint, a value the alter narrowed, or anything else
// that would otherwise be skipped or changed without an error.
func failOnCopyWarnings(ctx context.Context, conn *sql.Conn) {
	var count int
	if err := conn.QueryRowContext(ctx, "show count(*) warnings").Scan(&count); err != nil {
		log.Fatalf("counting the copy chunk's warnings: %v", err)
	}
	if count == 0 {
		return
	}
	var level, message string
	var code int
	if err := conn.QueryRowContext(ctx, "show warnings limit 1").Scan(&level, &code, &message); err != nil {
		log.Fatalf("copy chunk raised %d warning(s), and reading the first failed: %v", count, err)
	}
	log.Fatalf("copy chunk raised %d warning(s), the first is %s %d: %s", count, level, code, message)
}
