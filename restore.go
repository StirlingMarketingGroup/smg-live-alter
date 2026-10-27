package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

type foreignKey struct {
	Name       string // constraint name, unquoted
	Definition string // everything after "CONSTRAINT `name` ", e.g. "FOREIGN KEY (`a`) REFERENCES `p` (`id`)"
}

type originalTrigger struct {
	Name, SQLMode, Collation string
	Original                 string // SHOW CREATE TRIGGER, kept so an undo can put it back on the source
	Create                   string // header retargeted to the temp table
}

type inboundKey struct {
	Schema, Table, Name string
	Columns             []string // referenced columns of this table, in ORDINAL_POSITION order
}

type restorePlan struct {
	ForeignKeys []foreignKey
	Triggers    []originalTrigger // EVENT_MANIPULATION, ACTION_TIMING, ACTION_ORDER order
	Inbound     []inboundKey      // compared at the prompt
}

// one swap statement; the swap executes these and the recovery file renders them
type restoreStep struct {
	Purpose string
	SQL     string // complete, no placeholders
	Trigger bool   // rendered inside DELIMITER $$ … $$ in the recovery file

	// Set on the locked swap's forward steps so a failure can count how far it got.
	dropsOriginal bool
	addsOriginal  bool
	renames       bool
}

// cutOverAtomic renames under LOCK TABLES. cutOverDropFirst drops the original
// table first. Foreign keys follow a renamed table, and a foreign key name is
// unique in the schema, so a table that has any cannot be swapped with a rename.
type cutOverStrategy int

const (
	cutOverDropFirst cutOverStrategy = iota
	cutOverAtomic
)

// A longer metadata-lock wait would queue every other query behind the swap.
const cutOverLockWaitSeconds = 10

func addForeignKeys(table string, foreignKeys []foreignKey, named bool) string {
	clauses := make([]string, len(foreignKeys))
	for i, fk := range foreignKeys {
		clauses[i] = "add " + fk.Definition
		if named {
			clauses[i] = "add constraint " + quoteIdent(fk.Name) + " " + fk.Definition
		}
	}
	return "alter table " + quoteIdent(table) + " " + strings.Join(clauses, ", ")
}

func sessionModeStep(sessionSQLMode, sessionCollation string) restoreStep {
	return restoreStep{
		Purpose: "restoring session sql_mode and collation",
		SQL:     "set session sql_mode=" + quoteString(sessionSQLMode) + ", collation_connection=" + quoteString(sessionCollation),
	}
}

func triggerModeStep(tr originalTrigger) restoreStep {
	return restoreStep{
		Purpose: "setting original trigger " + tr.Name + " sql_mode and collation",
		SQL:     "set session sql_mode=" + quoteString(tr.SQLMode) + ", collation_connection=" + quoteString(tr.Collation),
	}
}

func (p restorePlan) originalTriggerSteps() []restoreStep {
	out := make([]restoreStep, 0, len(p.Triggers)*2)
	for _, tr := range p.Triggers {
		out = append(out, triggerModeStep(tr))
		out = append(out, restoreStep{
			Purpose: "adding original trigger " + tr.Name,
			SQL:     tr.Create,
			Trigger: true,
		})
	}
	return out
}

func (p restorePlan) steps(tableName, tempTableName, sessionSQLMode, sessionCollation string) []restoreStep {
	var out []restoreStep
	if len(p.ForeignKeys) > 0 {
		out = append(out, restoreStep{Purpose: "adding foreign keys", SQL: addForeignKeys(tempTableName, p.ForeignKeys, true)})
	}
	out = append(out, p.originalTriggerSteps()...)
	out = append(out, sessionModeStep(sessionSQLMode, sessionCollation))
	out = append(out, restoreStep{
		Purpose: "renaming the temp table",
		SQL:     "alter table " + quoteIdent(tempTableName) + " rename " + quoteIdent(tableName),
	})
	return out
}

// atomicSteps drops every original trigger before creating any of them: trigger
// names are unique in the schema. The mirror triggers are not in the plan, so
// they stay on the source and leave with it in the rename.
func (p restorePlan) atomicSteps(tableName, tempTableName, oldTableName, runTableName, sessionSQLMode, sessionCollation string) []restoreStep {
	out := make([]restoreStep, 0, len(p.Triggers)*3+3)
	for _, tr := range p.Triggers {
		out = append(out, restoreStep{
			Purpose:       "dropping original trigger " + tr.Name,
			SQL:           "drop trigger " + quoteIdent(tr.Name),
			dropsOriginal: true,
		})
	}
	n := len(out)
	out = append(out, p.originalTriggerSteps()...)
	for i := n; i < len(out); i++ {
		if out[i].Trigger {
			out[i].addsOriginal = true
		}
	}
	out = append(out, sessionModeStep(sessionSQLMode, sessionCollation))
	out = append(out, restoreStep{
		Purpose: "renaming the tables",
		SQL:     "rename table " + quoteIdent(tableName) + " to " + quoteIdent(oldTableName) + ", " + quoteIdent(tempTableName) + " to " + quoteIdent(tableName),
		renames: true,
	})
	// The run record must not outlive the rename: the source and temp schemas are gone.
	out = append(out, restoreStep{
		Purpose: "dropping the run table",
		SQL:     "drop table " + quoteIdent(runTableName),
	})
	return out
}

// undoSteps reverses a locked swap that has not renamed yet. dropped originals
// left the source; created originals sit on the temp table. Names are
// schema-wide, so the temp copies are dropped before any source trigger returns.
func (p restorePlan) undoSteps(dropped, created int, sessionSQLMode, sessionCollation string) []restoreStep {
	var out []restoreStep
	for i := 0; i < created; i++ {
		tr := p.Triggers[i]
		out = append(out, restoreStep{
			Purpose: "dropping original trigger " + tr.Name + " from the temp table",
			SQL:     "drop trigger if exists " + quoteIdent(tr.Name),
		})
	}
	for i := 0; i < dropped; i++ {
		tr := p.Triggers[i]
		out = append(out, triggerModeStep(tr))
		out = append(out, restoreStep{
			Purpose: "restoring original trigger " + tr.Name,
			SQL:     tr.Original,
			Trigger: true,
		})
	}
	out = append(out, sessionModeStep(sessionSQLMode, sessionCollation))
	return out
}

// atomicRecoverySteps is the whole undo, for the file written before the lock.
// Each original is dropped first because it may already sit on the temp table.
func (p restorePlan) atomicRecoverySteps(sessionSQLMode, sessionCollation string) []restoreStep {
	out := make([]restoreStep, 0, len(p.Triggers)*3+1)
	for _, tr := range p.Triggers {
		out = append(out, restoreStep{
			Purpose: "dropping original trigger " + tr.Name,
			SQL:     "drop trigger if exists " + quoteIdent(tr.Name),
		})
		out = append(out, triggerModeStep(tr))
		out = append(out, restoreStep{
			Purpose: "restoring original trigger " + tr.Name,
			SQL:     tr.Original,
			Trigger: true,
		})
	}
	out = append(out, sessionModeStep(sessionSQLMode, sessionCollation))
	return out
}

// chooseCutOver picks the locked rename only when nothing references the table
// by foreign key and RENAME TABLE of a write-locked table is available (8.0.13).
// An unparsable version is drop-first.
func chooseCutOver(plan restorePlan, version string) (cutOverStrategy, string) {
	if len(plan.ForeignKeys) != 0 || len(plan.Inbound) != 0 {
		return cutOverDropFirst, "the table has foreign keys"
	}
	if !mysqlVersionAtLeast(version, 8, 0, 13) {
		return cutOverDropFirst, "the server is older than 8.0.13"
	}
	return cutOverAtomic, ""
}

func parseMySQLVersion(s string) (major, minor, patch int, ok bool) {
	next := func(rest string) (n int, after string, ok bool) {
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, rest, false
		}
		n, err := strconv.Atoi(rest[:i])
		if err != nil {
			return 0, rest, false
		}
		return n, rest[i:], true
	}
	major, s, ok = next(s)
	if !ok || s == "" || s[0] != '.' {
		return 0, 0, 0, false
	}
	minor, s, ok = next(s[1:])
	if !ok || s == "" || s[0] != '.' {
		return 0, 0, 0, false
	}
	patch, _, ok = next(s[1:])
	if !ok {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}

func mysqlVersionAtLeast(version string, major, minor, patch int) bool {
	a, b, c, ok := parseMySQLVersion(version)
	if !ok {
		return false
	}
	if a != major {
		return a > major
	}
	if b != minor {
		return b > minor
	}
	return c >= patch
}

func readRestorePlan(ctx context.Context, conn *sql.Conn, tableName, tempTableName string, foreignKeys []foreignKey, skip map[string]struct{}) (restorePlan, error) {
	p := restorePlan{ForeignKeys: foreignKeys}
	triggerRows, err := conn.QueryContext(ctx, "select`TRIGGER_NAME`,`SQL_MODE`,`COLLATION_CONNECTION`"+
		"from`information_schema`.`triggers`"+
		"where`TRIGGER_SCHEMA`=database()"+
		"and`EVENT_OBJECT_TABLE`=? "+
		"order by`EVENT_MANIPULATION`,`ACTION_TIMING`,`ACTION_ORDER`", tableName)
	if err != nil {
		return restorePlan{}, err
	}
	for triggerRows.Next() {
		var tr originalTrigger
		if err := triggerRows.Scan(&tr.Name, &tr.SQLMode, &tr.Collation); err != nil {
			triggerRows.Close()
			return restorePlan{}, err
		}
		if _, ok := skip[tr.Name]; ok {
			continue
		}
		p.Triggers = append(p.Triggers, tr)
	}
	if err := triggerRows.Err(); err != nil {
		triggerRows.Close()
		return restorePlan{}, err
	}
	if err := triggerRows.Close(); err != nil {
		return restorePlan{}, err
	}
	for i, tr := range p.Triggers {
		create, err := triggerCreateStatement(ctx, conn, tr.Name)
		if err != nil {
			return restorePlan{}, fmt.Errorf("trigger %s: %w", tr.Name, err)
		}
		rewritten, err := retargetTrigger(create, tr.Name, tempTableName)
		if err != nil {
			return restorePlan{}, fmt.Errorf("trigger %s: %w", tr.Name, err)
		}
		p.Triggers[i].Original = create
		p.Triggers[i].Create = rewritten
	}
	inbound, err := readInboundKeys(ctx, conn, tableName)
	if err != nil {
		return restorePlan{}, err
	}
	p.Inbound = inbound
	return p, nil
}

// The probe proves the foreign key definitions and the original trigger bodies
// compile against the altered columns. It does not prove the rename,
// permissions, or anything another session changes. The tool assumes no other
// DDL runs on the table or its foreign key neighbors during a run; the prompt
// re-check is a cheap guard for that assumption, not a proof.
func (p restorePlan) validate(ctx context.Context, conn *sql.Conn, tempTableName, probeTableName string) (err error) {
	if _, err := conn.ExecContext(ctx, "drop table if exists "+quoteIdent(probeTableName)); err != nil {
		return err
	}
	var fkChecks int
	var sessionSQLMode, sessionCollation string
	if err := conn.QueryRowContext(ctx, "select @@session.foreign_key_checks, @@session.sql_mode, @@session.collation_connection").
		Scan(&fkChecks, &sessionSQLMode, &sessionCollation); err != nil {
		return err
	}
	defer func() {
		if _, restoreErr := conn.ExecContext(ctx, "set foreign_key_checks=?, sql_mode=?, collation_connection=?", fkChecks, sessionSQLMode, sessionCollation); restoreErr != nil && err == nil {
			err = restoreErr
		}
	}()
	defer func() {
		if _, dropErr := conn.ExecContext(ctx, "drop table if exists "+quoteIdent(probeTableName)); dropErr != nil && err == nil {
			err = dropErr
		}
	}()
	if _, err := conn.ExecContext(ctx, "set foreign_key_checks=0"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "create table "+quoteIdent(probeTableName)+" like "+quoteIdent(tempTableName)); err != nil {
		return err
	}
	if len(p.ForeignKeys) > 0 {
		// unnamed: constraint names are schema-wide and the source still holds them
		if _, err := conn.ExecContext(ctx, addForeignKeys(probeTableName, p.ForeignKeys, false)); err != nil {
			if len(p.ForeignKeys) == 1 {
				return fmt.Errorf("foreign key %q: %w", p.ForeignKeys[0].Name, err)
			}
			names := make([]string, len(p.ForeignKeys))
			for i, fk := range p.ForeignKeys {
				names[i] = fk.Name
			}
			return fmt.Errorf("foreign keys %s: %w", strings.Join(names, ", "), err)
		}
	}
	for i, tr := range p.Triggers {
		if _, err := conn.ExecContext(ctx, "set session sql_mode=?, collation_connection=?", tr.SQLMode, tr.Collation); err != nil {
			return fmt.Errorf("trigger %q: %w", tr.Name, err)
		}
		probeName := "smgla_probe_" + strconv.Itoa(i)
		create, err := retargetTrigger(tr.Create, probeName, probeTableName)
		if err != nil {
			return fmt.Errorf("trigger %q: %w", tr.Name, err)
		}
		if _, err := conn.ExecContext(ctx, create); err != nil {
			return fmt.Errorf("trigger %q: %w", tr.Name, err)
		}
	}
	return nil
}

func (p restorePlan) equal(q restorePlan) bool {
	if len(p.ForeignKeys) != len(q.ForeignKeys) || len(p.Triggers) != len(q.Triggers) || len(p.Inbound) != len(q.Inbound) {
		return false
	}
	for i := range p.ForeignKeys {
		if p.ForeignKeys[i] != q.ForeignKeys[i] {
			return false
		}
	}
	for i := range p.Triggers {
		if p.Triggers[i] != q.Triggers[i] {
			return false
		}
	}
	for i := range p.Inbound {
		a, b := p.Inbound[i], q.Inbound[i]
		if a.Schema != b.Schema || a.Table != b.Table || a.Name != b.Name || len(a.Columns) != len(b.Columns) {
			return false
		}
		for j := range a.Columns {
			if a.Columns[j] != b.Columns[j] {
				return false
			}
		}
	}
	return true
}

func recoverySQL(database, charset string, steps []restoreStep, disableForeignKeyChecks bool) string {
	var b strings.Builder
	b.WriteString("use ")
	b.WriteString(quoteIdent(database))
	b.WriteString(";\nset names ")
	b.WriteString(quoteIdent(charset))
	b.WriteString(";\n")
	// The locked swap has no foreign keys, so its file does not touch this.
	if disableForeignKeyChecks {
		b.WriteString("set foreign_key_checks=0;\n")
	}
	for _, s := range steps {
		if s.Trigger {
			b.WriteString("DELIMITER $$\n")
			b.WriteString(s.SQL)
			if !strings.HasSuffix(s.SQL, "\n") {
				b.WriteByte('\n')
			}
			b.WriteString("$$\nDELIMITER ;\n")
			continue
		}
		b.WriteString(s.SQL)
		if !strings.HasSuffix(s.SQL, ";") {
			b.WriteByte(';')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func execRestoreStep(ctx context.Context, conn *sql.Conn, step restoreStep, tableName, tempTableName, recoveryPath string) {
	if _, err := conn.ExecContext(ctx, step.SQL); err != nil {
		log.Fatalf("%s failed: %v; %s is dropped and its rows are in %s; finish by running the steps in %s from this one on (the done: lines above list the ones that ran)", step.Purpose, err, tableName, tempTableName, recoveryPath)
	}
	log.Printf("done: %s", step.Purpose)
}

// swapUnderLock holds the table lock for the whole rename. Other sessions wait
// on it instead of seeing a missing name. The run table is locked because it
// is dropped before the unlock. A failure before the rename is undone while
// the lock is still held; the mirror triggers never moved, so a rerun resumes
// at the swap prompt.
func swapUnderLock(ctx context.Context, conn *sql.Conn, plan restorePlan, steps []restoreStep, tableName, tempTableName, oldTableName, runTableName, sessionSQLMode, sessionCollation, recoveryPath string) {
	var lockWait int
	if err := conn.QueryRowContext(ctx, "select @@session.lock_wait_timeout").Scan(&lockWait); err != nil {
		log.Fatalf("could not lock %s: %v; nothing changed, rerun to try the swap again", tableName, err)
	}
	if lockWait > cutOverLockWaitSeconds {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("set session lock_wait_timeout=%d", cutOverLockWaitSeconds)); err != nil {
			log.Fatalf("could not lock %s: %v; nothing changed, rerun to try the swap again", tableName, err)
		}
	}
	lockSQL := "lock tables " + quoteIdent(tableName) + " write, " + quoteIdent(tempTableName) + " write, " + quoteIdent(runTableName) + " write"
	if _, err := conn.ExecContext(ctx, lockSQL); err != nil {
		log.Fatalf("could not lock %s: %v; nothing changed, rerun to try the swap again", tableName, err)
	}

	dropped, created := 0, 0
	renamed := false
	failedAfterRename := false
	for _, step := range steps {
		if _, err := conn.ExecContext(ctx, step.SQL); err != nil {
			if renamed {
				log.Printf("%s failed: %v; %s is swapped and %s still holds the previous table", step.Purpose, err, tableName, oldTableName)
				failedAfterRename = true
				break
			}
			undoLockedSwap(ctx, conn, plan.undoSteps(dropped, created, sessionSQLMode, sessionCollation), tableName, recoveryPath, step.Purpose, err)
		}
		log.Printf("done: %s", step.Purpose)
		if step.dropsOriginal {
			dropped++
		}
		if step.addsOriginal {
			created++
		}
		if step.renames {
			renamed = true
		}
	}

	cleanupFailed := failedAfterRename
	if _, err := conn.ExecContext(ctx, "unlock tables"); err != nil {
		log.Printf("unlocking tables after swapping %s: %v", tableName, err)
		cleanupFailed = true
	}
	if lockWait > cutOverLockWaitSeconds {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("set session lock_wait_timeout=%d", lockWait)); err != nil {
			log.Printf("restoring lock_wait_timeout after swapping %s: %v", tableName, err)
			cleanupFailed = true
		}
	}
	if _, err := conn.ExecContext(ctx, "drop table "+quoteIdent(oldTableName)); err != nil {
		log.Printf("dropping %s: %v", oldTableName, err)
		cleanupFailed = true
	}
	if cleanupFailed {
		// os.Exit skips the deferred close, and a live session would keep the lock.
		conn.Close()
		os.Exit(0)
	}
}

func undoLockedSwap(ctx context.Context, conn *sql.Conn, steps []restoreStep, tableName, recoveryPath, purpose string, cause error) {
	var undoErr error
	for _, step := range steps {
		if _, undoErr = conn.ExecContext(ctx, step.SQL); undoErr != nil {
			break
		}
	}
	if _, err := conn.ExecContext(ctx, "unlock tables"); err != nil && undoErr == nil {
		// The triggers are already back. Closing the session releases the lock if this failed.
		log.Printf("unlocking tables after undoing the swap of %s: %v", tableName, err)
	}
	conn.Close()
	if undoErr != nil {
		log.Fatalf("%s failed: %v, and undoing it failed: %v; %s may be missing original triggers. Run %s to put them back on %s, then rerun to try the swap again",
			purpose, cause, undoErr, tableName, recoveryPath, tableName)
	}
	log.Fatalf("%s failed: %v; the swap was undone and %s has its triggers again; rerun to try the swap again",
		purpose, cause, tableName)
}

func readInboundKeys(ctx context.Context, conn *sql.Conn, tableName string) ([]inboundKey, error) {
	rows, err := conn.QueryContext(ctx, "select `TABLE_SCHEMA`,`TABLE_NAME`,`CONSTRAINT_NAME`,`REFERENCED_COLUMN_NAME`"+
		" from `information_schema`.`KEY_COLUMN_USAGE`"+
		" where `REFERENCED_TABLE_SCHEMA`=database()"+
		" and `REFERENCED_TABLE_NAME`=?"+
		" and not (`TABLE_SCHEMA`=database() and `TABLE_NAME`=?)"+
		" order by `TABLE_SCHEMA`,`TABLE_NAME`,`CONSTRAINT_NAME`,`ORDINAL_POSITION`", tableName, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []inboundKey
	for rows.Next() {
		var schema, table, name, col string
		if err := rows.Scan(&schema, &table, &name, &col); err != nil {
			return nil, err
		}
		if n := len(keys); n > 0 && keys[n-1].Schema == schema && keys[n-1].Table == table && keys[n-1].Name == name {
			keys[n-1].Columns = append(keys[n-1].Columns, col)
			continue
		}
		keys = append(keys, inboundKey{Schema: schema, Table: table, Name: name, Columns: []string{col}})
	}
	return keys, rows.Err()
}

func selfReferentialForeignKeys(ctx context.Context, conn *sql.Conn, tableName string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, "select distinct `CONSTRAINT_NAME`"+
		" from `information_schema`.`KEY_COLUMN_USAGE`"+
		" where `TABLE_SCHEMA`=database()"+
		" and `TABLE_NAME`=?"+
		" and `REFERENCED_TABLE_SCHEMA`=database()"+
		" and `REFERENCED_TABLE_NAME`=?"+
		" order by `CONSTRAINT_NAME`", tableName, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func tableEngine(ctx context.Context, conn *sql.Conn, tableName string) (string, error) {
	var engine string
	err := conn.QueryRowContext(ctx, "select `ENGINE` from `information_schema`.`tables` where `TABLE_SCHEMA`=database() and `TABLE_NAME`=?", tableName).Scan(&engine)
	return engine, err
}

func checkInboundKeys(keys []inboundKey, mapping columnMapping, source, dest []column, destIndexes []tableIndex) error {
	destOf := make(map[string]string, len(mapping))
	for _, p := range mapping {
		destOf[p.Source] = p.Destination
	}
	sourceByName := make(map[string]column, len(source))
	for _, c := range source {
		sourceByName[c.ColumnName] = c
	}
	destByName := make(map[string]column, len(dest))
	for _, c := range dest {
		destByName[c.ColumnName] = c
	}
	for _, key := range keys {
		for _, col := range key.Columns {
			destName, ok := destOf[col]
			if !ok {
				return fmt.Errorf("inbound foreign key %q on %s.%s: referenced column %q is not on the destination", key.Name, key.Schema, key.Table, col)
			}
			if destName != col {
				return fmt.Errorf("inbound foreign key %q on %s.%s: referenced column %q maps to %q", key.Name, key.Schema, key.Table, col, destName)
			}
			srcCol := sourceByName[col]
			dstCol := destByName[col]
			if srcCol.ColumnType != dstCol.ColumnType {
				return fmt.Errorf("inbound foreign key %q on %s.%s: referenced column %q changes from %s to %s", key.Name, key.Schema, key.Table, col, srcCol.ColumnType, dstCol.ColumnType)
			}
			if srcCol.CharacterSetName != dstCol.CharacterSetName {
				return fmt.Errorf("inbound foreign key %q on %s.%s: referenced column %q changes character set from %q to %q", key.Name, key.Schema, key.Table, col, srcCol.CharacterSetName, dstCol.CharacterSetName)
			}
			if srcCol.CollationName != dstCol.CollationName {
				return fmt.Errorf("inbound foreign key %q on %s.%s: referenced column %q changes collation from %q to %q", key.Name, key.Schema, key.Table, col, srcCol.CollationName, dstCol.CollationName)
			}
		}
		if !destIndexCoversInbound(destIndexes, key.Columns) {
			return fmt.Errorf("inbound foreign key %q on %s.%s: the destination has no index whose leading columns are (%s) with no prefix length", key.Name, key.Schema, key.Table, strings.Join(key.Columns, ", "))
		}
	}
	return nil
}

func destIndexCoversInbound(indexes []tableIndex, cols []string) bool {
	if len(cols) == 0 {
		return false
	}
	for _, idx := range indexes {
		if len(idx.Parts) < len(cols) {
			continue
		}
		ok := true
		for i, col := range cols {
			part := idx.Parts[i]
			if part.Expression != "" || part.Column != col || part.SubPart != 0 {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
