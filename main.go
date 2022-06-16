package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	_ "github.com/go-sql-driver/mysql"
	"gopkg.in/cheggaaa/pb.v1"
)

func main() {
	usernamePtr := flag.String("u", "root", "your MySQL username")
	passwordPtr := flag.String("p", "", "your MySQL password")
	hostPtr := flag.String("h", "localhost", "your MySQL host")
	portPtr := flag.Int("P", 3306, "your MySQL port")
	databasePtr := flag.String("d", "", "your MySQL database")

	// bytesPtr := flag.Int("b", 8*1024*1024, "the approximate chunk size in bytes")

	queryPtr := flag.String("q", "", "the alter table query to be executed")
	prefixPtr := flag.String("prefix", "_SMGLA_", "the prefix of the new tmp table")

	flag.Parse()

	if len(*queryPtr) == 0 {
		f, err := os.CreateTemp("", "")
		if err != nil {
			log.Fatal("failed to open default text editor:", err)
		}
		f.Close()
		cmd := exec.Command("nano", f.Name())
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Start()
		if err != nil {
			log.Fatal("failed to start editing command:", err)
		}
		err = cmd.Wait()
		if err != nil {
			log.Fatal("error while editing:", err)
		}
		b, err := os.ReadFile(f.Name())
		if err != nil {
			log.Fatal("failed to read file:", err)
		}
		*queryPtr = strings.TrimSpace(string(b))
		fmt.Println(*queryPtr)
	}

	db, err := sql.Open("mysql", *usernamePtr+":"+*passwordPtr+"@tcp("+*hostPtr+":"+strconv.Itoa(*portPtr)+")/"+*databasePtr+"?charset=utf8mb4&collation=utf8mb4_unicode_ci&multiStatements=true")
	if err != nil {
		log.Fatal("Error connecting to db:", err)
	}
	db.SetMaxOpenConns(1)

	err = db.Ping()
	if err != nil {
		log.Fatal("Error connecting to db:", err)
	}

	parseAlterRegex := regexp.MustCompile("(?i)alter\\s+table\\s*(?:`?([^`]+)`?\\.)?`?([^`]+)`?\\s*([^;]+)")
	m := parseAlterRegex.FindStringSubmatch(*queryPtr)
	if len(m) != 4 {
		log.Fatal("Couldn't parse alter query, is it valid?")
	}

	schema := m[1]
	if len(schema) == 0 {
		schema = *databasePtr
	}
	table := m[2]
	tableQ := "`" + table + "`"
	alter := m[3]

	log.Println("Starting work on", tableQ)
	log.Println("Getting create", tableQ)
	createData, err := db.Query("show create table`" + table + "`;")
	if err != nil {
		log.Fatal("Failed to get create statement:", err)
	}
	var create string
	var x interface{}
	for createData.Next() {
		err = createData.Scan(&x, &create)
		if err != nil {
			log.Fatal("Failed to scan rows:", err)
		}
	}
	if len(create) == 0 {
		log.Fatal("Couldn't get create statement for table:", err)
	}

	log.Println("Getting columns")
	columnsData, err := db.Query("select`COLUMN_NAME`" +
		"from`INFORMATION_SCHEMA`.`COLUMNS`" +
		"where`TABLE_NAME`='" + table + "'" +
		"and`TABLE_SCHEMA`='" + schema + "'" +
		"and`EXTRA`not in('VIRTUAL GENERATED','STORED GENERATED')" +
		"order by`ordinal_position`;")
	if err != nil {
		log.Fatal("Failed to get columns for table:", err)
	}

	columns := make(map[string]string, 0)
	var c string
	for columnsData.Next() {
		err = columnsData.Scan(&c)
		if err != nil {
			log.Fatal("Failed to scan columns:", err)
		}
		columns[c] = c
	}

	parseChangeColumnRegex := regexp.MustCompile("(?i)change\\s+column\\s*`?([^`]+)`?\\s+`?([^`]+)`?")
	changedColumns := parseChangeColumnRegex.FindAllStringSubmatch(alter, -1)
	for _, m := range changedColumns {
		if m[1] != m[2] {
			columns[m[1]] = m[2]
		}
	}

	parseDropColumnRegex := regexp.MustCompile("(?i)drop\\s+column\\s*`?([^`]+)`?")
	droppedColumns := parseDropColumnRegex.FindAllStringSubmatch(alter, -1)
	for _, m := range droppedColumns {
		delete(columns, m[1])
	}

	var oldColumns, oldNewColumns, newColumns, updateColumns string
	var i int
	for o, n := range columns {
		if i != 0 {
			oldColumns += ","
			oldNewColumns += ","
			newColumns += ","
			updateColumns += ","
		}
		oldColumns += "`" + o + "`"
		oldNewColumns += "new.`" + o + "`"
		newColumns += "`" + n + "`"
		updateColumns += "`" + n + "`=new.`" + o + "`"
		i++
	}

	log.Println("Getting primary key(s)")
	primaryKeysData, err := db.Query("select`COLUMN_NAME`" +
		"from`INFORMATION_SCHEMA`.`STATISTICS`" +
		"where table_name='" + table + "'" +
		"and`INDEX_NAME`='PRIMARY'" +
		"and`INDEX_SCHEMA`='" + schema + "'")
	if err != nil {
		log.Fatal("failed to select primary keys:", err)
	}

	var newPrimaryKeys, newHexPrimaryKeys, newPrimaryKeysDesc, oldPrimaryKeys, oldOldPrimaryKeys string
	i = 0
	for primaryKeysData.Next() {
		err = primaryKeysData.Scan(&c)
		if err != nil {
			log.Fatal("Failed to scan primary keys:", err)
		}
		if i != 0 {
			newPrimaryKeys += ","
			newHexPrimaryKeys += ","
			newPrimaryKeysDesc += ","
			oldPrimaryKeys += ","
			oldOldPrimaryKeys += ","
		}
		newPrimaryKeys += "`" + columns[c] + "`"
		newHexPrimaryKeys += "hex(`" + columns[c] + "`)"
		newPrimaryKeysDesc += "`" + columns[c] + "`desc"
		oldPrimaryKeys += "`" + c + "`"
		oldOldPrimaryKeys += "old.`" + c + "`"

		i++
	}
	primaryKeys := make([]interface{}, i)
	primaryKeyPtrs := make([]interface{}, i)
	for i := range primaryKeys {
		primaryKeyPtrs[i] = &primaryKeys[i]
	}

	parseConstraintsRegex := regexp.MustCompile("(?i),\\s*(constraint\\s*`?[^`]+`?\\s*foreign\\s+key\\s*\\([^)]+\\)\\s*references\\s?`?[^`]+`?\\s*\\([^)]+\\)\\s*[a-z ]*)")
	constraints := parseConstraintsRegex.FindAllStringSubmatch(create, -1)
	create = parseConstraintsRegex.ReplaceAllString(create, "")

	restoreConstraints := "alter table" + tableQ
	for i, c := range constraints {
		if i != 0 {
			restoreConstraints += ","
		}
		restoreConstraints += "add " + c[1]
	}

	newTable := *prefixPtr + table
	newTableQ := "`" + newTable + "`"
	log.Println("Dropping table", newTableQ, "(if exists)")
	_, err = db.Exec("drop table if exists`" + newTable + "`;")
	if err != nil {
		log.Fatal("Failed to remove table:", err)
	}

	newInsertsTable := *prefixPtr + "inserts_" + table
	newInsertsTableQ := "`" + newInsertsTable + "`"
	log.Println("Dropping table", newInsertsTableQ, "(if exists)")
	_, err = db.Exec("drop table if exists" + newInsertsTableQ)
	if err != nil {
		log.Fatal("Failed to remove table:", err)
	}

	createTableRegex := regexp.MustCompile("(?i)create\\s+table\\s*`?[^`]+`?")
	create = createTableRegex.ReplaceAllString(create, "create table`"+newTable+"`")

	log.Println("Getting triggers")
	triggersData, err := db.Query("select`TRIGGER_NAME`" +
		"from`information_schema`.`triggers`" +
		"where`EVENT_OBJECT_TABLE`='" + table + "'" +
		"and`TRIGGER_NAME`not like'" + *prefixPtr + "%'" +
		"and`EVENT_OBJECT_SCHEMA`='" + schema + "';")
	if err != nil {
		log.Fatal("Failed to get triggers:", err)
	}
	triggers := make([]string, 0)
	var t string
	for triggersData.Next() {
		err = triggersData.Scan(&t)
		if err != nil {
			log.Fatal("Failed to scan trigger:", err)
		}
		triggers = append(triggers, t)
	}
	createTriggersRegex := regexp.MustCompile("(?i)\\s+on\\s*`" + regexp.QuoteMeta(table) + "`")
	createTriggers := make([]string, len(triggers))
	for i, t := range triggers {
		createTriggersData, err := db.Query("show create trigger`" + t + "`;")
		if err != nil {
			log.Fatal("Failed to get trigger:", err)
		}
		for createTriggersData.Next() {
			err = createTriggersData.Scan(&x, &x, &t, &x, &x, &x, &x)
			if err != nil {
				log.Fatal("Failed to scan trigger:", err)
			}
			createTriggers[i] = strings.Replace(t, createTriggersRegex.FindString(t), " on"+newTableQ, 1)
		}
	}

	log.Println("Disabling foreign keys, unique keys")
	_, err = db.Exec("set foreign_key_checks=0;set unique_checks=0;")
	if err != nil {
		log.Fatal("Failed to disable:", err)
	}

	log.Println("Creating new table", newTableQ)
	_, err = db.Exec(create)
	if err != nil {
		log.Fatal("Failed to create new table:", err)
	}

	log.Println("Applying alter table to", newTableQ)
	_, err = db.Exec("alter table" + newTableQ + alter)
	if err != nil {
		log.Fatal("Failed to alter new table:", err)
	}

	log.Println("Creating new table", newInsertsTableQ)
	_, err = db.Exec("create table" + newInsertsTableQ + "like" + newTableQ)
	if err != nil {
		log.Fatal("Failed to create new table:", err)
	}

	insertTriggerQ := "`" + *prefixPtr + table + "_AFTER_INSERT`"
	updateTriggerQ := "`" + *prefixPtr + table + "_AFTER_UPDATE`"
	deleteTriggerQ := "`" + *prefixPtr + table + "_AFTER_DELETE`"

	log.Println("Dropping trigger", insertTriggerQ, "(if exists)")
	_, err = db.Exec("drop trigger if exists" + insertTriggerQ)
	if err != nil {
		log.Fatal("Failed to drop trigger:", err)
	}

	log.Println("Dropping trigger", updateTriggerQ, "(if exists)")
	_, err = db.Exec("drop trigger if exists" + updateTriggerQ)
	if err != nil {
		log.Fatal("Failed to drop trigger:", err)
	}

	log.Println("Dropping trigger", deleteTriggerQ, "(if exists)")
	_, err = db.Exec("drop trigger if exists" + deleteTriggerQ)
	if err != nil {
		log.Fatal("Failed to drop trigger:", err)
	}

	log.Println("Adding", insertTriggerQ)
	_, err = db.Exec("create trigger" + insertTriggerQ + "after insert on" + tableQ + "for each row " +
		"begin " +
		"insert into" + newInsertsTableQ + "(" + newColumns + ")values(" + oldNewColumns + ");" +
		"end")
	if err != nil {
		log.Fatal("Failed to add trigger:", err)
	}

	log.Println("Adding", updateTriggerQ)
	_, err = db.Exec("create trigger" + updateTriggerQ + "after update on" + tableQ + "for each row " +
		"begin " +
		"update" + newTableQ + "set" + updateColumns + "where(" + newPrimaryKeys + ")=(" + oldOldPrimaryKeys + ");" +
		"update" + newInsertsTableQ + "set" + updateColumns + "where(" + newPrimaryKeys + ")=(" + oldOldPrimaryKeys + ");" +
		"end")
	if err != nil {
		log.Fatal("Failed to add trigger:", err)
	}

	log.Println("Adding", deleteTriggerQ)
	_, err = db.Exec("create trigger" + deleteTriggerQ + "after delete on" + tableQ + "for each row " +
		"begin " +
		"delete from" + newTableQ + "where(" + newPrimaryKeys + ")=(" + oldOldPrimaryKeys + ");" +
		"delete from" + newInsertsTableQ + "where(" + newPrimaryKeys + ")=(" + oldOldPrimaryKeys + ");" +
		"end")
	if err != nil {
		log.Fatal("Failed to add trigger:", err)
	}

	log.Println("Getting count")
	countData, err := db.Query("select count(*)from" + tableQ)
	if err != nil {
		log.Fatal("Failed to get count:", err)
	}
	var count int
	for countData.Next() {
		err = countData.Scan(&count)
		if err != nil {
			log.Fatal("Failed to scan count:", err)
		}
	}

	const target = time.Millisecond * 200
	const min = 8
	var elapsed time.Duration

	log.Println("Inserting data")
	bar := pb.StartNew(count)
	limit := 1024
	i = 0
	for {
		q := "insert ignore into" + newTableQ + "(" + newColumns + ")select" + oldColumns + "from" + tableQ
		if i != 0 {
			q += "where(" + oldPrimaryKeys + ")>("
			maxData, err := db.Query("select" + newPrimaryKeys + "from" + newTableQ + "order by" + newPrimaryKeysDesc + " limit 1")
			if err != nil {
				log.Fatal("Failed to get max primary keys:", err)
			}
			for maxData.Next() {
				err = maxData.Scan(primaryKeyPtrs...)
				if err != nil {
					log.Fatal("Failed to scan max primary keys:", err)
				}
			}
			for i := range primaryKeys {
				if i != 0 {
					q += ","
				}
				q += "?"
			}
			q += ")"
		}
		q += "order by" + oldPrimaryKeys + "limit " + strconv.Itoa(limit)

		if elapsed != 0 {
			limit = int(target.Seconds() / elapsed.Seconds() * float64(limit))
		}

		if limit < min {
			limit = min
		}

		start := time.Now()
		if i == 0 {
			_, err = db.Exec(q)
		} else {
			_, err = db.Exec(q, primaryKeys...)
		}
		if err != nil {
			log.Fatal("Failed to insert rows:", err)
		}
		elapsed = time.Since(start)
		// fmt.Println(limit, elapsed, target.Seconds()/elapsed.Seconds(), target.Seconds()/elapsed.Seconds()*float64(limit))

		rowCountData, err := db.Query("select row_count();")
		if err != nil {
			log.Fatal("Failed to get row count:", err)
		}
		var rowCount int
		for rowCountData.Next() {
			err = rowCountData.Scan(&rowCount)
			if err != nil {
				log.Fatal("Failed to scan row count:", err)
			}
		}

		if rowCount == 0 {
			bar.Finish()
			log.Println("Finished copying data")
			break
		}

		bar.Add(rowCount)
		i += rowCount
	}

	// This definitely creates more downtime where the table just doesn't exist
	// But it also creates the least room for potential problems, i.e. rows being added in between the
	// copy from the inserts table and the dropping of the orders table. If it doesn't exist,
	// then rows can't be inserted into it and get lost, and (hopefully) the application can
	// retry it's failed insert/update/select once newTable is renamed
	log.Println("Dropping old table")
	_, err = db.Exec("drop table" + tableQ)
	if err != nil {
		log.Fatal("Failed to drop table:", err)
	}

	log.Println("Inserting from", newInsertsTableQ)
	_, err = db.Exec("insert ignore into" + newTableQ + "(" + newColumns + ")select" + newColumns + "from" + newInsertsTableQ)
	if err != nil {
		log.Fatal("Failed to clone data:", err)
	}

	log.Println("Restoring triggers")
	for _, t := range createTriggers {
		_, err = db.Exec(t)
		if err != nil {
			color.Set(color.FgYellow)
			defer color.Unset()
			log.Println("Failed to add trigger:", err)
			fmt.Println(t)
		}
	}

	log.Println("Renaming tables")
	_, err = db.Exec("rename table" + newTableQ + "to" + tableQ)
	if err != nil {
		log.Fatal("Failed to rename table:", err)
	}

	log.Println("Restoring constraints")
	_, err = db.Exec(restoreConstraints)
	if err != nil {
		log.Fatal("Failed to alter table:", err)
	}

	log.Println("Dropping table", newInsertsTableQ)
	_, err = db.Exec("drop table" + newInsertsTableQ)
	if err != nil {
		log.Fatal("Failed to drop table:", err)
	}

}
