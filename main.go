package main

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"time"

	cool "github.com/StirlingMarketingGroup/cool-mysql"
	"github.com/fatih/color"
	"github.com/posener/cmd"
	"github.com/vbauerster/mpb/v5"
	"github.com/vbauerster/mpb/v5/decor"
)

var confDir, _ = os.UserConfigDir()

var (
	root = cmd.New()

	connectionsFile = root.String("c", confDir+"/smgla/connections.yaml", "your connections file")

	// not entirely sure how much this really affects performance,
	// since the performance bottleneck is almost guaranteed to be writing
	// the rows to the source
	rowBufferSize = root.Int("r", 50, "max rows buffer size. Will have this many rows downloaded and ready for importing")

	tempTableSuffix = root.String("suffix", "_smgla_", "suffix of the temp table used for initial creation before the swap and drop")

	args = root.Args("connection", "connection, ex:\n"+
		"smg-live-alter [flags] 'user:pass@(host)/dbname'\n\n"+
		"see: https://github.com/go-sql-driver/mysql#dsn-data-source-name\n\n"+
		"Or, optionally, you can use your connections in your connections file like so:\n\n"+
		"smg-live-alter [flags] localhost")
)

func main() {
	start := time.Now()

	// parse our command line arguments and make sure we
	// were given something that makes sense
	root.ParseArgs(os.Args...)
	if len(*args) < 1 {
		root.Usage()
		os.Exit(1)
	}

	dbDSN := (*args)[0]

	// lookup connection information in the users config file
	// for much easier and shorter (and probably safer) command usage
	if connections, err := getConnections(*connectionsFile); err == nil {
		if c, ok := connections[dbDSN]; ok {
			dbDSN = connectionToDSN(c)
		}
	}

	// source connection is the first argument
	// this is where our rows are coming from
	db, err := cool.NewFromDSN(dbDSN, dbDSN)
	if err != nil {
		panic(err)
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

	pb := mpb.New()

	// and get the count, so we can show our swick progress bars
	log.Println("getting row count")
	var count struct {
		Count int64
	}
	err = db.Select(&count, "select count(*)`Count`from`"+tableName+"`", 0)
	if err != nil {
		panic(err)
	}

	log.Println("getting table creation statement")
	// now we get the table creation syntax from our source
	var table struct {
		CreateMySQL string `mysql:"Create Table"`
	}
	err = db.Select(&table, "show create table`"+tableName+"`", 0)
	if err != nil {
		panic(err)
	}

	tempTableName := tableName + *tempTableSuffix

	// delete the table from our destination
	log.Println("dropping temp table (if it exists)")
	err = db.Exec("drop table if exists`" + tempTableName + "`")
	if err != nil {
		panic(err)
	}

	// since foreign key constraints have globally unique names (for some reason)
	// we can't just create our temp table with constraints because
	// the names will likely conflict with the table that already exists

	// so we will strip the constraints here and add them back once we're done
	var constraints string

	// we can safely assume the constraints start like this because you can't have
	// constraints without columns!
	constraintsStart := strings.Index(table.CreateMySQL, ",\n  CONSTRAINT ")
	if constraintsStart != -1 {
		// we have the start of our constraints block, and since mysql
		// always (hopefully) gives them in a block, we can find the last
		// constraint and everything in the middle is what we want
		constraintsEnd := strings.LastIndex(table.CreateMySQL, ",\n  CONSTRAINT ")

		// but we need the end of the line, so we'll get the byte index of the newline
		// after our last index as our end marker
		constraintsEnd = constraintsEnd + strings.IndexByte(table.CreateMySQL[constraintsEnd+2:], '\n') + 2

		// then we can keep track of our constraints so we can add them back
		// to our table once we've dropped the original table
		constraints = table.CreateMySQL[constraintsStart:constraintsEnd]

		// and store our create query without our constraints
		table.CreateMySQL = table.CreateMySQL[:constraintsStart] + table.CreateMySQL[constraintsEnd:]
	}

	// now we can make the table on our destination
	log.Println("creating temp table")
	err = db.Exec("CREATE TABLE `" + tempTableName + "`" + strings.TrimPrefix(table.CreateMySQL, "CREATE TABLE `"+tableName+"`"))
	if err != nil {
		panic(err)
	}

	log.Println("applying alter to temp table")
	err = db.Exec(fmt.Sprintf("alter table`%s`%s", tempTableName, alterPart))
	if err != nil {
		panic(err)
	}

	selectColumns := new(strings.Builder)

	oldColumns, err := getTableColumns(db, tableName)
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
	newColumnsSet := columnsSet(newColumns)
	newColumnsIntersect := make(map[string]struct{})
	oldPrimaryColumns := make([]*column, 0)
	newPrimaryColumns := make([]*column, 0)

	i := 0
	for _, c := range oldColumns {
		newColumnName := oldColumnsMap[c.ColumnName]

		if _, ok := newColumnsSet[newColumnName]; !ok {
			continue
		}

		if i != 0 {
			selectColumns.WriteByte(',')
		}
		selectColumns.WriteString(fmt.Sprintf("`%s` `%s`", c.ColumnName, newColumnName))

		newColumnsIntersect[newColumnName] = struct{}{}

		if c.PrimaryKey {
			oldPrimaryColumns = append(oldPrimaryColumns, c)
		}

		oldColumns[i] = c
		i++
	}
	oldColumns = oldColumns[:i]

	i = 0
	for _, c := range newColumns {
		if _, ok := newColumnsIntersect[c.ColumnName]; !ok {
			continue
		}

		newColumns[i] = c
		i++

		if c.PrimaryKey {
			newPrimaryColumns = append(newPrimaryColumns, c)
		}
	}
	newColumns = newColumns[:i]

	insertTrigger := tableName + "_after_insert" + *tempTableSuffix
	log.Println("dropping insert trigger (if it exists)")
	err = db.Exec("drop trigger if exists`" + insertTrigger + "`")
	if err != nil {
		panic(err)
	}
	log.Println("creating insert trigger")
	insert := fmt.Sprintf("insert ignore into`%s`(%s)values(%s)", tempTableName, quoteColumns(newColumns), quoteColumnsPrefix(oldColumns, "new."))
	err = db.Exec("create trigger`" + insertTrigger + "`after insert on`" + tableName + "`for each row " + insert)
	if err != nil {
		panic(err)
	}

	updateTrigger := tableName + "_after_update" + *tempTableSuffix
	log.Println("dropping update trigger (if it exists)")
	err = db.Exec("drop trigger if exists`" + updateTrigger + "`")
	if err != nil {
		panic(err)
	}
	log.Println("creating update trigger")
	updateBld := new(strings.Builder)
	for i, c := range oldColumns {
		if i != 0 {
			updateBld.WriteByte(',')
		}
		updateBld.WriteByte('`')
		updateBld.WriteString(oldColumnsMap[c.ColumnName])
		updateBld.WriteByte('`')

		updateBld.WriteByte('=')

		updateBld.WriteString("new.")
		updateBld.WriteByte('`')
		updateBld.WriteString(c.ColumnName)
		updateBld.WriteByte('`')
	}
	update := fmt.Sprintf("update`%s`set%s where(%s)=(%s)", tempTableName, updateBld.String(), quoteColumns(newPrimaryColumns), quoteColumnsPrefix(oldPrimaryColumns, "new."))
	err = db.Exec("create trigger`" + updateTrigger + "`after update on`" + tableName + "`for each row begin\n" + insert + ";\n" + update + ";\nend")
	if err != nil {
		panic(err)
	}

	deleteTrigger := tableName + "_after_delete" + *tempTableSuffix
	log.Println("dropping delete trigger (if it exists)")
	err = db.Exec("drop trigger if exists`" + deleteTrigger + "`")
	if err != nil {
		panic(err)
	}
	log.Println("creating delete trigger")
	delete := fmt.Sprintf("delete from`%s`where(%s)=(%s);", tempTableName, quoteColumns(newPrimaryColumns), quoteColumnsPrefix(oldPrimaryColumns, "old."))
	err = db.Exec("create trigger`" + deleteTrigger + "`after delete on`" + tableName + "`for each row " + delete)
	if err != nil {
		panic(err)
	}

	newRowStruct, err := tableRowStruct(newColumns)
	if err != nil {
		panic(err)
	}

	// this gets the "type" of our struct from our dynamic struct
	structType := reflect.Indirect(reflect.ValueOf(newRowStruct.Build().New())).Type()
	// and then we make a channel with reflection for our new type of struct
	chRef := reflect.MakeChan(reflect.ChanOf(reflect.BothDir, structType), *rowBufferSize)
	ch := chRef.Interface()

	// oh yeah, that's just one query. We don't actually have to chunk this selection
	// because we're dealing with rows as they come in, instead of trying to select them
	// all into memory or something first, which makes this code dramatically simpler
	// and should work with tables of all sizes
	go func() {
		defer chRef.Close()

		log.Println("selecting all the rows!")
		err := db.Select(ch, "select /*+ MAX_EXECUTION_TIME(2147483647) */ "+selectColumns.String()+"from`"+tableName+"`", 0)
		if err != nil {
			panic(err)
		}
	}()

	// our pretty bar config for the progress bars
	// their documentation lives over here https://github.com/vbauerster/mpb
	bar := pb.AddBar(count.Count,
		mpb.BarStyle("|▇▇ |"),
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

	targetChunkTime := 500 * time.Millisecond
	chunkStartTime := time.Now()

	// start the import!
	// Now this *does* have to be chunked because there's no way to stream
	// rows to mysql, but cool mysql handles this for us, all it needs is the same
	// channel we got from the select
	err = db.I().SetAfterChunkExec(func(start time.Time) {
		chunkTime := time.Since(chunkStartTime)
		if chunkTime > targetChunkTime {
			db.MaxInsertSize.Set(int(float64(db.MaxInsertSize.Get()) * float64(targetChunkTime) / float64(chunkTime)))
		}

		bar.Increment()
		bar.DecoratorEwmaUpdate(time.Since(start))

		chunkStartTime = time.Now()
	}).Insert("insert ignore into`"+tempTableName+"`", ch)
	if err != nil {
		panic(err)
	}

	// and just in case the rows have changed count since our count selection,
	// we'll just tell the progress bar that we're finished
	bar.SetTotal(bar.Current(), true)

	tx, cancel, err := db.BeginTx()
	defer cancel()
	if err != nil {
		panic(err)
	}

	// stop foreign key checks
	log.Println("disabling foreign key checks for our connection")
	err = tx.Exec("set foreign_key_checks=0")
	if err != nil {
		panic(err)
	}

	// but we can't forget our triggers!
	// lets grab the triggers from the source table and make sure
	// we re-create them all on our destination
	log.Println("getting original triggers")
	var triggers []*struct {
		Trigger     string
		CreateMySQL string `mysql:"SQL Original Statement"`
	}
	err = db.Select(&triggers, fmt.Sprintf("show triggers where`Table`like'%s'and not`Trigger`like'%%%s'", tableName, *tempTableSuffix), 0)
	if err != nil {
		panic(err)
	}
	for _, r := range triggers {
		err := db.Select(r, "show create trigger`"+r.Trigger+"`", 0)
		if err != nil {
			panic(err)
		}
	}

	// drop the old table now that our temp table is done
	log.Println("dropping the original table")
	err = tx.Exec("drop table if exists`" + tableName + "`")
	if err != nil {
		panic(err)
	}

	// no we can add back our constraints if we have them
	// converting our constraints to alter table syntax by removing our leading
	// comma and adding the word "add" at the beginning of each line
	if len(constraints) != 0 {
		log.Println("adding constraints")
		err = tx.Exec("alter table`" + tempTableName + "`" + strings.ReplaceAll(strings.TrimLeft(constraints, ","), "\n", "\nadd"))
		if err != nil {
			panic(err)
		}
	}

	for _, r := range triggers {
		log.Println("adding original triggers")
		err = tx.Exec(renameTriggerTable(r.CreateMySQL, tempTableName))
		if err != nil {
			panic(err)
		}
	}

	// rename our temp table to the real table name
	// we could do an atomic rename here, but the problem is that atomic renames
	// also rename all the constraints of other tables pointing to our original table, and
	// we want those constraints to point to our new table instead

	// if you're doing this live, there *is* some down time, but other tools handle this the same
	// way, so I don't think it's unreasonable if we do the same
	log.Println("renaming temp table")
	err = tx.Exec("alter table`" + tempTableName + "`rename`" + tableName + "`")
	if err != nil {
		panic(err)
	}

	err = tx.Commit()
	if err != nil {
		panic(err)
	}

	fmt.Println("finished altering", tableName, "in", time.Since(start))
}
