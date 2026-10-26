package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"

	mysql "github.com/go-sql-driver/mysql"
)

// The table option moves on every insert. The column attribute has no "=n", so it stays.
var autoIncrementOption = regexp.MustCompile(` AUTO_INCREMENT=\d+`)

func normalizeCreate(create string) string {
	return autoIncrementOption.ReplaceAllString(create, "")
}

func runTableDDL(name string, key []column) string {
	b := new(strings.Builder)
	fmt.Fprintf(b, "create table %s (", quoteIdent(name))
	b.WriteString("`alter_text` longtext not null,")
	b.WriteString("`source_create` longtext not null,")
	b.WriteString("`temp_create` longtext not null,")
	for i, c := range key {
		fmt.Fprintf(b, "`k%d` %s", i, c.ColumnType)
		if c.CharacterSetName != "" {
			fmt.Fprintf(b, " character set %s collate %s", c.CharacterSetName, c.CollationName)
		}
		b.WriteByte(',')
	}
	b.WriteString("`copy_done` tinyint not null default 0,")
	b.WriteString("`readers` text not null default ('')")
	b.WriteString(") engine=InnoDB")
	return b.String()
}

func insertRunSQL(name string) string {
	return "insert into " + quoteIdent(name) + " (`alter_text`,`source_create`,`temp_create`) values (?,?,?)"
}

func checkpointSQL(name string, n int) string {
	sets := make([]string, n)
	for i := 0; i < n; i++ {
		sets[i] = fmt.Sprintf("`k%d`=@smgla_e%d", i, i)
	}
	return "update " + quoteIdent(name) + " set " + strings.Join(sets, ",")
}

func markCopyDoneSQL(name string) string {
	return "update " + quoteIdent(name) + " set `copy_done`=1"
}

func restoreCursorSQL(name string, n int) string {
	cols := make([]string, n)
	vars := make([]string, n)
	for i := 0; i < n; i++ {
		cols[i] = fmt.Sprintf("`k%d`", i)
		vars[i] = fmt.Sprintf("@smgla_k%d", i)
	}
	return "select " + strings.Join(cols, ",") + " into " + strings.Join(vars, ",") + " from " + quoteIdent(name)
}

func clearReadersSQL(name string) string {
	return "update " + quoteIdent(name) + " set `readers`=''"
}

// A missing run table is a fresh start, not a failure.
func stoppedRunFor(ctx context.Context, conn *sql.Conn, runTable, alterText string) (bool, error) {
	var stored string
	err := conn.QueryRowContext(ctx, "select `alter_text` from "+quoteIdent(runTable)).Scan(&stored)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1146 {
			return false, nil
		}
		return false, err
	}
	if stored != alterText {
		log.Println("found a stopped run of a different alter; starting fresh")
		return false, nil
	}
	return true, nil
}

type resumeNames struct {
	table         string
	temp          string
	run           string
	deleteTrigger string
	updateTrigger string
	insertTrigger string
}

type resumeState struct {
	copyDone  bool
	hasCursor bool
	readers   []string
}

type runColumn struct {
	Name       string
	ColumnType string
	Charset    string
	Collation  string
}

type triggerRow struct {
	Name      string
	Table     string
	Timing    string
	Event     string
	Statement string
	SQLMode   string
}

type wantTrigger struct {
	Name  string
	Event string
	Body  string
}

// resumeFacts is everything the resume decision compares. Tests pass it in directly.
type resumeFacts struct {
	names              resumeNames
	runExists          bool
	runEngine          string
	hasRow             bool
	alterText          string
	storedAlter        string
	storedSourceCreate string
	sourceCreateNow    string
	tempExists         bool
	storedTempCreate   string
	tempCreateNow      string
	keyColumns         []runColumn
	traversalKey       []column
	triggers           []triggerRow
	wantTriggers       []wantTrigger
	sqlMode            string
	copyDone           bool
	cursorNULL         bool
	readers            string
}

func decideResume(f resumeFacts) (resumeState, error) {
	table := f.names.table
	if !f.runExists || !f.hasRow {
		return resumeState{}, fmt.Errorf("cannot resume %s: no run record for %s; rerun with -fresh to start over", table, f.names.temp)
	}
	if f.runEngine != "InnoDB" {
		return resumeState{}, fmt.Errorf("cannot resume %s: run table %s uses %s, not InnoDB; rerun with -fresh to start over", table, f.names.run, f.runEngine)
	}
	if f.storedAlter != f.alterText {
		return resumeState{}, fmt.Errorf("cannot resume %s: the alter text differs from the saved run; rerun with -fresh to start over", table)
	}
	if normalizeCreate(f.storedSourceCreate) != normalizeCreate(f.sourceCreateNow) {
		return resumeState{}, fmt.Errorf("cannot resume %s: the source table changed since the run started; rerun with -fresh to start over", table)
	}
	if !f.tempExists {
		return resumeState{}, fmt.Errorf("cannot resume %s: temp table %s does not exist; rerun with -fresh to start over", table, f.names.temp)
	}
	if normalizeCreate(f.storedTempCreate) != normalizeCreate(f.tempCreateNow) {
		return resumeState{}, fmt.Errorf("cannot resume %s: temp table %s changed since the run started; rerun with -fresh to start over", table, f.names.temp)
	}
	if len(f.keyColumns) != len(f.traversalKey) {
		return resumeState{}, fmt.Errorf("cannot resume %s: saved cursor columns are %d, primary key columns are %d; rerun with -fresh to start over", table, len(f.keyColumns), len(f.traversalKey))
	}
	for i, c := range f.traversalKey {
		want := fmt.Sprintf("k%d", i)
		got := f.keyColumns[i]
		if got.Name != want || got.ColumnType != c.ColumnType || got.Charset != c.CharacterSetName || got.Collation != c.CollationName {
			return resumeState{}, fmt.Errorf("cannot resume %s: cursor column %s (%s, character set %q, collation %q) does not match primary key column %s (%s, character set %q, collation %q); rerun with -fresh to start over",
				table, got.Name, got.ColumnType, got.Charset, got.Collation, c.ColumnName, c.ColumnType, c.CharacterSetName, c.CollationName)
		}
	}

	// A resumed copy is only correct if these triggers have mirrored every write
	// since the run's first chunk. Matching bodies prove they are this alter's
	// triggers, and the tool's no-other-DDL assumption covers the rest.
	found := make(map[string]triggerRow, len(f.triggers))
	for _, tr := range f.triggers {
		found[tr.Name] = tr
	}
	for _, want := range f.wantTriggers {
		got, ok := found[want.Name]
		if !ok {
			return resumeState{}, fmt.Errorf("cannot resume %s: mirror trigger %s is missing; rerun with -fresh to start over", table, want.Name)
		}
		if got.Table != table || got.Timing != "AFTER" || got.Event != want.Event {
			return resumeState{}, fmt.Errorf("cannot resume %s: mirror trigger %s is not an after %s trigger on %s; rerun with -fresh to start over", table, want.Name, strings.ToLower(want.Event), table)
		}
		if got.Statement != want.Body {
			return resumeState{}, fmt.Errorf("cannot resume %s: mirror trigger %s body differs; rerun with -fresh to start over", table, want.Name)
		}
		if got.SQLMode != f.sqlMode {
			return resumeState{}, fmt.Errorf("cannot resume %s: mirror trigger %s sql_mode is %q, this session is %q; rerun with -fresh to start over", table, want.Name, got.SQLMode, f.sqlMode)
		}
	}

	return resumeState{
		copyDone:  f.copyDone,
		hasCursor: !f.cursorNULL,
		readers:   parseReaders(f.readers),
	}, nil
}

func parseReaders(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func loadRunForResume(ctx context.Context, conn *sql.Conn, names resumeNames, alterText string, sourceCreateNow string, traversalKey []column, triggerBodies map[string]string, triggerSQLMode string) (resumeState, error) {
	f := resumeFacts{
		names:           names,
		alterText:       alterText,
		sourceCreateNow: sourceCreateNow,
		traversalKey:    traversalKey,
		sqlMode:         triggerSQLMode,
		wantTriggers: []wantTrigger{
			{Name: names.deleteTrigger, Event: "DELETE", Body: triggerBodies[names.deleteTrigger]},
			{Name: names.updateTrigger, Event: "UPDATE", Body: triggerBodies[names.updateTrigger]},
			{Name: names.insertTrigger, Event: "INSERT", Body: triggerBodies[names.insertTrigger]},
		},
	}

	engine, err := tableEngine(ctx, conn, names.run)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return decideResume(f)
		}
		return resumeState{}, err
	}
	f.runExists = true
	f.runEngine = engine

	var copyDone int
	err = conn.QueryRowContext(ctx, "select `alter_text`,`source_create`,`temp_create`,`copy_done`,`readers` from "+quoteIdent(names.run)).
		Scan(&f.storedAlter, &f.storedSourceCreate, &f.storedTempCreate, &copyDone, &f.readers)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return decideResume(f)
		}
		return resumeState{}, err
	}
	f.hasRow = true
	f.copyDone = copyDone != 0

	tempCreate, tempExists, err := showCreateIfExists(ctx, conn, names.temp)
	if err != nil {
		return resumeState{}, err
	}
	f.tempExists = tempExists
	f.tempCreateNow = tempCreate

	cols, err := runCursorColumns(ctx, conn, names.run)
	if err != nil {
		return resumeState{}, err
	}
	f.keyColumns = cols

	// k0 exists only when the cursor columns match; a mismatch is reported from those columns.
	if len(cols) == len(traversalKey) && len(cols) > 0 && cols[0].Name == "k0" {
		if err := conn.QueryRowContext(ctx, "select `k0` is null from "+quoteIdent(names.run)).Scan(&f.cursorNULL); err != nil {
			return resumeState{}, err
		}
	}

	triggers, err := mirrorTriggerRows(ctx, conn, names)
	if err != nil {
		return resumeState{}, err
	}
	f.triggers = triggers
	return decideResume(f)
}

func showCreateIfExists(ctx context.Context, conn *sql.Conn, table string) (create string, exists bool, err error) {
	var name string
	err = conn.QueryRowContext(ctx, "show create table "+quoteIdent(table)).Scan(&name, &create)
	if err == nil {
		return create, true, nil
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1146 {
		return "", false, nil
	}
	return "", false, err
}

func runCursorColumns(ctx context.Context, conn *sql.Conn, runTable string) ([]runColumn, error) {
	rows, err := conn.QueryContext(ctx, "select `COLUMN_NAME`,`COLUMN_TYPE`,ifnull(`CHARACTER_SET_NAME`,''),ifnull(`COLLATION_NAME`,'')"+
		" from `information_schema`.`columns`"+
		" where `TABLE_SCHEMA`=database() and `TABLE_NAME`=? and `COLUMN_NAME` regexp '^k[0-9]+$'"+
		" order by `ORDINAL_POSITION`", runTable)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []runColumn
	for rows.Next() {
		var c runColumn
		if err := rows.Scan(&c.Name, &c.ColumnType, &c.Charset, &c.Collation); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func mirrorTriggerRows(ctx context.Context, conn *sql.Conn, names resumeNames) ([]triggerRow, error) {
	rows, err := conn.QueryContext(ctx, "select `TRIGGER_NAME`,`EVENT_OBJECT_TABLE`,`ACTION_TIMING`,`EVENT_MANIPULATION`,`ACTION_STATEMENT`,`SQL_MODE`"+
		" from `information_schema`.`triggers`"+
		" where `TRIGGER_SCHEMA`=database() and `TRIGGER_NAME` in (?,?,?)",
		names.deleteTrigger, names.updateTrigger, names.insertTrigger)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var triggers []triggerRow
	for rows.Next() {
		var tr triggerRow
		if err := rows.Scan(&tr.Name, &tr.Table, &tr.Timing, &tr.Event, &tr.Statement, &tr.SQLMode); err != nil {
			return nil, err
		}
		triggers = append(triggers, tr)
	}
	return triggers, rows.Err()
}

func saveRunReaders(ctx context.Context, conn *sql.Conn, runTable string, ids []string) error {
	_, err := conn.ExecContext(ctx, "update "+quoteIdent(runTable)+" set `readers`=?", strings.Join(ids, ","))
	return err
}
