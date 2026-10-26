package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	mysql "github.com/StirlingMarketingGroup/cool-mysql"
	"github.com/fatih/color"
	"github.com/posener/cmd"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

var confDir, _ = os.UserConfigDir()

// The chunk target is capped to bound each copy statement's duration and the share locks it holds.
const maxChunkRows = 10000

var (
	root = cmd.New()

	connectionsFile = root.String("c", confDir+"/smgla/connections.yaml", "your connections file")

	rowBufferSize = root.Int("r", 1000, "starting rows per copy chunk; adjusted to keep each chunk near 500ms")

	tempTableSuffix = root.String("suffix", "_smgla_", "suffix of the temp table used for initial creation before the swap and drop")

	verbose = root.Bool("v", false, "writes the full query log to stdout")

	args = root.Args("connection", "connection, ex:\n"+
		"smg-live-alter [flags] 'user:pass@(host)/dbname'\n\n"+
		"see: https://github.com/go-sql-driver/mysql#dsn-data-source-name\n\n"+
		"Or, optionally, you can use your connections in your connections file like so:\n\n"+
		"smg-live-alter [flags] localhost")
)

func main() {
	start := time.Now()

	root.ParseArgs(os.Args...)
	if len(*args) < 1 {
		root.Usage()
		os.Exit(1)
	}
	if *rowBufferSize < 1 || *rowBufferSize > maxChunkRows {
		log.Fatalf("-r is %d; the starting chunk must be 1 to %d rows", *rowBufferSize, maxChunkRows)
	}

	dbDSN := (*args)[0]

	if connections, err := getConnections(*connectionsFile); err == nil {
		if c, ok := connections[dbDSN]; ok {
			dbDSN = connectionToDSN(c)
		}
	}

	db, err := mysql.NewFromDSN(dbDSN, dbDSN)
	if err != nil {
		panic(err)
	}

	if *verbose {
		db.Log = func(detail mysql.LogDetail) {
			log.Println(detail.Query)
		}
	}

	db.DisableUnusedColumnWarnings = true

	alterQuery, err := promptText()
	if err != nil {
		panic(err)
	}

	m := parseAlterRegexp.FindStringSubmatch(alterQuery)
	if len(m) != 4 {
		panic("couldn't parse alter query, is it valid?")
	}

	tableName := m[2]
	alterPart := m[3]

	hr := strings.Repeat("+", 64)
	log.Printf("using alter query:\n%s\n%s\n%s\n", hr, color.CyanString(alterQuery), hr)

	log.Println("getting row count")
	var count struct {
		Count int64
	}
	err = db.Select(&count, "select count(*)`Count`from"+quoteIdent(tableName), 0)
	if err != nil {
		panic(err)
	}

	log.Println("getting table creation statement")
	var table struct {
		CreateMySQL string `mysql:"Create Table"`
	}
	err = db.Select(&table, "show create table "+quoteIdent(tableName), 0)
	if err != nil {
		panic(err)
	}

	// the traversal key is the source primary key in index order. its values
	// live in user variables for the whole copy, so refuse types those
	// variables can't compare the way the index does, before any temp table
	// or trigger exists.
	oldColumns, err := getTableColumns(db, tableName)
	if err != nil {
		panic(err)
	}
	traversalKey := primaryKeyColumns(oldColumns)
	if err := checkTraversalKeyTypes(traversalKey); err != nil {
		log.Fatalf("cannot copy %s: %v", tableName, err)
	}

	// One session for the session settings (sql_mode, foreign_key_checks, the
	// copy's isolation level, and the @smgla_* cursor variables) and for
	// lookups that bind table names as values. cool-mysql's Select does not
	// take ? placeholders.
	ctx := context.Background()
	conn, err := db.Reads.Conn(ctx)
	if err != nil {
		log.Fatalf("taking a connection: %v", err)
	}
	defer conn.Close()

	exec := func(purpose, query string, args ...any) sql.Result {
		res, err := conn.ExecContext(ctx, query, args...)
		if err != nil {
			log.Fatalf("%s: %v", purpose, err)
		}
		return res
	}

	// A cascading foreign key changes these rows without firing triggers, so the temp table can't follow it.
	referential, err := queryReferentialConstraints(ctx, conn, tableName)
	if err != nil {
		panic(err)
	}
	for _, fk := range referential {
		for _, rule := range []string{fk.UpdateRule, fk.DeleteRule} {
			if referentialActionSkipsTriggers(rule) {
				log.Fatalf("cannot copy %s: foreign key %q uses %s; MySQL changes the source rows for that action without firing triggers, so the temp table can't follow them", tableName, fk.Name, rule)
			}
		}
	}
	selfRefs, err := selfReferentialForeignKeys(ctx, conn, tableName)
	if err != nil {
		panic(err)
	}
	for _, name := range selfRefs {
		log.Fatalf("cannot copy %s: foreign key %q references the table itself; the probe would resolve it against the old schema, so this alter can't be proven", tableName, name)
	}
	tempTableName := tableName + *tempTableSuffix
	probeTableName := tableName + *tempTableSuffix + "probe_"
	if len(probeTableName) > 64 {
		log.Fatalf("cannot copy %s: probe table name %q is %d characters; MySQL identifiers are at most 64", tableName, probeTableName, len(probeTableName))
	}

	insertTrigger := tableName + "_after_insert" + *tempTableSuffix
	updateTrigger := tableName + "_after_update" + *tempTableSuffix
	deleteTrigger := tableName + "_after_delete" + *tempTableSuffix
	skipMirrors := map[string]struct{}{
		insertTrigger: {},
		updateTrigger: {},
		deleteTrigger: {},
	}

	// a run that stopped part way leaves its triggers writing into the temp
	// table, so they go before the temp table does: in the other order every
	// write to the source fails until the triggers are recreated
	for _, trigger := range []string{insertTrigger, updateTrigger, deleteTrigger} {
		log.Println("dropping trigger", trigger, "(if it exists)")
		if err := db.Exec("drop trigger if exists " + quoteIdent(trigger)); err != nil {
			panic(err)
		}
	}

	log.Println("dropping temp table (if it exists)")
	err = db.Exec("drop table if exists " + quoteIdent(tempTableName))
	if err != nil {
		panic(err)
	}

	log.Println("dropping probe table (if it exists)")
	err = db.Exec("drop table if exists " + quoteIdent(probeTableName))
	if err != nil {
		panic(err)
	}

	// Constraint names are unique to the whole schema, so the temp table can't
	// reuse the original's. Foreign keys are stripped here and added back once
	// the original table is dropped (adding one doesn't rebuild the table with
	// foreign key checks off). A generated check <table>_chk_<n> is created as
	// <temp>_chk_<n>, which MySQL renames back with the table. A custom check
	// name is dropped so MySQL generates one.
	foreignKeys, createMySQL, err := splitConstraints(table.CreateMySQL, tableName, tempTableName)
	if err != nil {
		log.Fatalf("cannot copy %s: %v", tableName, err)
	}
	table.CreateMySQL = createMySQL

	log.Println("creating temp table")
	err = db.Exec("CREATE TABLE " + quoteIdent(tempTableName) + strings.TrimPrefix(table.CreateMySQL, "CREATE TABLE "+quoteIdent(tableName)))
	if err != nil {
		panic(err)
	}

	log.Println("applying alter to temp table")
	err = db.Exec("alter table " + quoteIdent(tempTableName) + " " + rewriteCheckNames(alterPart, tableName, tempTableName))
	if err != nil {
		panic(err)
	}

	oldColumnsMap := make(map[string]string)
	for _, c := range oldColumns {
		oldColumnsMap[c.ColumnName] = c.ColumnName
	}
	changedColumns := parseChangeColumnRegex.FindAllStringSubmatch(alterPart, -1)
	for _, m := range changedColumns {
		if m[1] != m[2] {
			oldColumnsMap[m[1]] = m[2]
		}
	}

	newColumns, err := getTableColumns(db, tempTableName)
	if err != nil {
		panic(err)
	}

	// One ordered source→destination list. The copy and the three triggers write
	// this list, so an AFTER or FIRST reorder can't pair a destination column
	// with the wrong source column.
	mapping, err := buildColumnMapping(oldColumns, oldColumnsMap, newColumns)
	if err != nil {
		log.Fatalf("cannot copy %s: %v", tableName, err)
	}
	derived := derivedPairs(mapping, oldColumnsMap, oldColumns, newColumns)

	sourceIndexes, err := getUniqueIndexes(db, tableName)
	if err != nil {
		panic(err)
	}
	destIndexes, err := getUniqueIndexes(db, tempTableName)
	if err != nil {
		panic(err)
	}
	if err := checkUniqueCoverage(mapping, derived, oldColumns, newColumns, sourceIndexes, destIndexes); err != nil {
		log.Fatalf("cannot copy %s: %v", tableName, err)
	}
	destKey, err := destinationKey(mapping, newColumns)
	if err != nil {
		log.Fatalf("cannot copy %s: %v", tableName, err)
	}

	srcEngine, err := tableEngine(ctx, conn, tableName)
	if err != nil {
		panic(err)
	}
	tmpEngine, err := tableEngine(ctx, conn, tempTableName)
	if err != nil {
		panic(err)
	}
	if srcEngine != tmpEngine {
		log.Fatalf("cannot copy %s: storage engine changes from %s to %s", tableName, srcEngine, tmpEngine)
	}

	destAllIndexes, err := getIndexes(db, tempTableName)
	if err != nil {
		panic(err)
	}

	log.Println("reading the restore plan")
	plan, err := readRestorePlan(ctx, conn, tableName, tempTableName, foreignKeys, skipMirrors)
	if err != nil {
		log.Fatalf("cannot copy %s: %v", tableName, err)
	}
	if err := checkInboundKeys(plan.Inbound, mapping, oldColumns, newColumns, destAllIndexes); err != nil {
		log.Fatalf("cannot copy %s: %v", tableName, err)
	}
	if err := plan.validate(ctx, conn, tempTableName, probeTableName); err != nil {
		log.Fatalf("cannot copy %s: %v", tableName, err)
	}

	// A trigger keeps the sql_mode it was created with, and the application's
	// sessions use the server's global mode, so the triggers store exactly what
	// the application's writes will store after the swap.
	exec("setting the trigger sql_mode", "set session sql_mode=@@global.sql_mode")
	var mode string
	if err := conn.QueryRowContext(ctx, "select @@session.sql_mode").Scan(&mode); err != nil {
		log.Fatalf("reading the trigger sql_mode: %v", err)
	}
	log.Printf("mirror triggers use sql_mode %q", mode)

	insertSQL := fmt.Sprintf("insert into %s(%s)values(%s)", quoteIdent(tempTableName), mapping.insertList(), mapping.prefixedSources("new."))
	deleteSQL := fmt.Sprintf("delete from %s where(%s)=(%s)", quoteIdent(tempTableName), destKey.insertList(), destKey.prefixedSources("old."))
	// The application's update holds the source row's lock, so the copy cannot
	// insert that row between the existence check and the insert. An update that
	// changes no values reports 0 rows, so ROW_COUNT() cannot tell "no row" from
	// "row already matched".
	updateSQL := fmt.Sprintf("begin\nupdate %s set%s where(%s)=(%s);\nif not exists(select 1 from %s where(%s)=(%s)) then\n%s;\nend if;\nend",
		quoteIdent(tempTableName), mapping.assignments("new."), destKey.insertList(), destKey.prefixedSources("old."),
		quoteIdent(tempTableName), destKey.insertList(), destKey.prefixedSources("new."),
		insertSQL)

	// Delete, then update, then insert. A row the insert trigger writes can
	// already have its later updates and deletes mirrored.
	log.Println("creating delete trigger")
	exec("creating the delete trigger", "create trigger "+quoteIdent(deleteTrigger)+" after delete on "+quoteIdent(tableName)+" for each row "+deleteSQL)

	log.Println("creating update trigger")
	exec("creating the update trigger", "create trigger "+quoteIdent(updateTrigger)+" after update on "+quoteIdent(tableName)+" for each row "+updateSQL)

	log.Println("creating insert trigger")
	exec("creating the insert trigger", "create trigger "+quoteIdent(insertTrigger)+" after insert on "+quoteIdent(tableName)+" for each row "+insertSQL)

	p := mpb.New()

	bar := p.New(count.Count,
		mpb.BarStyle().Lbound("|").Filler("▇").Tip("▇").Padding(" ").Rbound("|"),
		mpb.PrependDecorators(
			decor.Name(color.HiBlueString(tableName)),
			decor.OnComplete(decor.Percentage(decor.WC{W: 5}), color.HiMagentaString(" done!")),
		),
		mpb.AppendDecorators(
			decor.CountersNoUnit("( "+color.HiCyanString("%d/%d")+", ", decor.WCSyncWidth),
			decor.AverageSpeed(-1, " "+color.HiGreenString("%.2f/s")+" ) ", decor.WCSyncWidth),
			decor.AverageETA(decor.ET_STYLE_MMSS),
		),
	)

	// read committed: lock in share mode then locks only the rows it copies,
	// not the gaps between them, so the application's inserts into a range
	// being copied don't wait (the insert trigger already sends those rows to
	// the temp table)
	exec("setting the copy isolation level", "set session transaction isolation level read committed")

	pkNames := columnNames(traversalKey)
	pks := quoteIdents(pkNames)
	after := keyAfter(pkNames)
	atOrBefore := keyAtOrBefore(pkNames)
	eVars := sessionVarList("@smgla_e", len(pkNames))
	advance := cursorAssign(len(pkNames))
	insertCols := mapping.insertList()
	noopUpdate := quoteIdent(tempTableName) + "." + quoteIdent(mapping[0].Destination) + "=" + quoteIdent(tempTableName) + "." + quoteIdent(mapping[0].Destination)
	selectCols := mapping.selectList()

	const targetChunkTime = 500 * time.Millisecond

	chunk := *rowBufferSize
	first := true
	log.Println("copying rows")
	for {
		exec("setting the chunk-found flag", "set @smgla_found=0")

		endKey := fmt.Sprintf("select /*+ MAX_EXECUTION_TIME(2147483647) */ 1,%s into @smgla_found,%s from %s", pks, eVars, quoteIdent(tableName))
		if !first {
			endKey += " where " + after
		}
		endKey += " order by " + pks + " limit 1 offset ?"
		exec("finding the chunk end key", endKey, chunk-1)

		var found int
		if err := conn.QueryRowContext(ctx, "select @smgla_found").Scan(&found); err != nil {
			log.Fatalf("reading the chunk-found flag: %v", err)
		}

		var where string
		if !first {
			where = after
		}
		if found == 1 {
			if where != "" {
				where += " and " + atOrBefore
			} else {
				where = atOrBefore
			}
		}

		copyStmt := fmt.Sprintf("insert into %s(%s) select %s from %s", quoteIdent(tempTableName), insertCols, selectCols, quoteIdent(tableName))
		if where != "" {
			copyStmt += " where " + where
		}
		// a duplicate is the row a trigger already wrote (checkUniqueCoverage
		// makes that the only kind), and the no-op keeps it without a warning,
		// so any warning the copy raises is a real problem
		copyStmt += " lock in share mode on duplicate key update " + noopUpdate

		started := time.Now()
		res := exec("copying a chunk", copyStmt)
		elapsed := time.Since(started)
		// Any warning stops the copy, which is stricter than the mirror triggers.
		// The copy governs rows that already exist; the triggers run under the
		// global sql_mode with no IGNORE, so a write the destination rejects
		// fails the application's statement.
		failOnCopyWarnings(ctx, conn)

		if found == 1 {
			bar.IncrBy(chunk)
		} else {
			affected, err := res.RowsAffected()
			if err != nil {
				log.Fatalf("reading the copied row count: %v", err)
			}
			bar.IncrBy(int(affected))
			break
		}

		exec("advancing the copy cursor", "set "+advance)
		first = false

		if elapsed <= 0 {
			elapsed = 1
		}
		grown := int64(chunk) * int64(targetChunkTime) / int64(elapsed)
		if elapsed > targetChunkTime {
			chunk = int(grown)
			if chunk < 1 {
				chunk = 1
			}
		} else {
			add := (grown - int64(chunk)) / 10
			if add > int64(maxChunkRows) {
				add = int64(maxChunkRows)
			}
			chunk += int(add)
			if chunk > maxChunkRows {
				chunk = maxChunkRows
			}
		}
	}

	// and just in case the rows have changed count since our count selection,
	// we'll just tell the progress bar that we're finished
	bar.SetTotal(bar.Current(), true)

	p.Wait()

	if !yesNo("do the drop/swap?") {
		os.Exit(0)
	}

	var createNowName, createNow string
	if err := conn.QueryRowContext(ctx, "show create table "+quoteIdent(tableName)).Scan(&createNowName, &createNow); err != nil {
		log.Fatalf("reading create table at swap: %v", err)
	}
	fksNow, _, err := splitConstraints(createNow, tableName, tempTableName)
	if err != nil {
		log.Fatalf("cannot copy %s: %v", tableName, err)
	}
	planNow, err := readRestorePlan(ctx, conn, tableName, tempTableName, fksNow, skipMirrors)
	if err != nil {
		log.Fatalf("cannot copy %s: %v", tableName, err)
	}
	if !plan.equal(planNow) {
		log.Fatalf("cannot copy %s: the source's triggers or foreign keys changed during the copy; rerun the tool", tableName)
	}

	var sessionSQLMode, sessionCollation, charset, database string
	if err := conn.QueryRowContext(ctx, "select @@session.sql_mode, @@session.collation_connection, @@character_set_client, database()").
		Scan(&sessionSQLMode, &sessionCollation, &charset, &database); err != nil {
		log.Fatalf("reading session sql_mode, collation, character set, and database: %v", err)
	}
	steps := plan.steps(tableName, tempTableName, sessionSQLMode, sessionCollation)
	recoveryPath := fmt.Sprintf("smgla-recovery-%s-%d.sql", tableName, time.Now().Unix())
	if err := os.WriteFile(recoveryPath, []byte(recoverySQL(database, charset, steps)), 0o644); err != nil {
		log.Fatalf("writing recovery file: %v", err)
	}
	log.Printf("wrote recovery file %s", recoveryPath)

	log.Println("disabling foreign key checks for our connection")
	exec("disabling foreign key checks", "set foreign_key_checks=0")

	log.Println("dropping the original table")
	exec("dropping the original table", "drop table if exists "+quoteIdent(tableName))

	for _, step := range steps {
		execRestoreStep(ctx, conn, step, tableName, tempTableName, recoveryPath)
	}

	log.Println("finished altering", tableName, "in", time.Since(start))
}
