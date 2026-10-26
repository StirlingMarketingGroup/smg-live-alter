package main

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
)

// MySQL rebuilds a trigger's header when it stores it, so the name and table
// are the quoted identifiers in TRIGGER … ON … FOR EACH ROW, whatever the body
// looks like. A header this pattern cannot parse (ANSI_QUOTES, for example)
// is an error, and nothing is installed.
var triggerHeaderRegexp = regexp.MustCompile("(?i)^(.*? TRIGGER )`(?:[^`]|``)+`( (?:BEFORE|AFTER) (?:INSERT|UPDATE|DELETE) ON )`(?:[^`]|``)+`( FOR EACH ROW)")

func retargetTrigger(create, triggerName, tableName string) (string, error) {
	m := triggerHeaderRegexp.FindStringSubmatchIndex(create)
	if m == nil {
		return "", fmt.Errorf("can't parse the trigger header: %.200s", create)
	}
	return create[:m[3]] + quoteIdent(triggerName) + create[m[4]:m[5]] + quoteIdent(tableName) + create[m[6]:], nil
}

func triggerCreateStatement(ctx context.Context, conn *sql.Conn, name string) (string, error) {
	var trigger, sqlMode, statement, charset, collation, databaseCollation string
	var created any
	err := conn.QueryRowContext(ctx, "show create trigger "+quoteIdent(name)).
		Scan(&trigger, &sqlMode, &statement, &charset, &collation, &databaseCollation, &created)
	return statement, err
}
