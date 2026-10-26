package main

import (
	"database/sql"
	"sort"

	mysql "github.com/StirlingMarketingGroup/cool-mysql"
)

type column struct {
	ColumnName string `mysql:"COLUMN_NAME"`
	Position   int    `mysql:"ORDINAL_POSITION"`
	DataType   string `mysql:"DATA_TYPE"`
	ColumnType string `mysql:"COLUMN_TYPE"`
	// CollationName is empty when the column has no collation. A change on a
	// unique column can make distinct values equal with no warning.
	CollationName        string `mysql:"COLLATION_NAME"`
	CharacterSetName     string `mysql:"CHARACTER_SET_NAME"`
	GenerationExpression string `mysql:"GENERATION_EXPRESSION"`
	PrimaryKey           bool
	SeqInIndex           int
}

func getTableColumns(db *mysql.Database, tableName string) ([]column, error) {
	var columns []column

	// we need to check to see if the db supports generated columns
	// if it doesn't, our query to get column info will fail
	columnInfoCols := "`COLUMN_NAME`,`ORDINAL_POSITION`,`DATA_TYPE`,`COLUMN_TYPE`,ifnull(`COLLATION_NAME`,'')`COLLATION_NAME`,ifnull(`CHARACTER_SET_NAME`,'')`CHARACTER_SET_NAME`"
	ok, err := db.Exists("select 0 "+
		"from`information_schema`.`columns`"+
		"where lower(`TABLE_SCHEMA`)='information_schema'"+
		"and lower(`table_name`)='columns'"+
		"and lower(`column_name`)='generation_expression'", 0)
	if err != nil {
		return nil, err
	}
	if ok {
		columnInfoCols += ",`GENERATION_EXPRESSION`"
	}

	err = db.Select(&columns, "select"+columnInfoCols+
		"from`INFORMATION_SCHEMA`.`columns`"+
		"where`TABLE_SCHEMA`=database()"+
		"and`table_name`="+quoteString(tableName)+
		"order by`ORDINAL_POSITION`", 0)
	if err != nil {
		return nil, err
	}

	var primaryKeys []struct {
		ColumnName string `mysql:"Column_name"`
		SeqInIndex int    `mysql:"Seq_in_index"`
	}
	err = db.Select(&primaryKeys, "show index from "+quoteIdent(tableName)+" where `Key_name`='PRIMARY'", 0)
	if err != nil {
		return nil, err
	}

	primaryKeySeq := make(map[string]int, len(primaryKeys))
	for _, pk := range primaryKeys {
		primaryKeySeq[pk.ColumnName] = pk.SeqInIndex
	}

	for i := range columns {
		c := &columns[i]
		if seq, ok := primaryKeySeq[c.ColumnName]; ok {
			c.PrimaryKey = true
			c.SeqInIndex = seq
		}
	}

	return columns, nil
}

type indexRow struct {
	KeyName    string         `mysql:"Key_name"`
	SeqInIndex int            `mysql:"Seq_in_index"`
	ColumnName sql.NullString `mysql:"Column_name"`
	SubPart    sql.NullInt64  `mysql:"Sub_part"`
	Expression sql.NullString `mysql:"Expression"`
}

func getUniqueIndexes(db *mysql.Database, tableName string) ([]tableIndex, error) {
	return readIndexes(db, "show index from "+quoteIdent(tableName)+" where `Non_unique`=0")
}

func getIndexes(db *mysql.Database, tableName string) ([]tableIndex, error) {
	return readIndexes(db, "show index from "+quoteIdent(tableName))
}

func readIndexes(db *mysql.Database, query string) ([]tableIndex, error) {
	var rows []indexRow
	err := db.Select(&rows, query, 0)
	if err != nil {
		return nil, err
	}

	order := make([]string, 0)
	grouped := make(map[string][]int)
	for i, r := range rows {
		if _, ok := grouped[r.KeyName]; !ok {
			order = append(order, r.KeyName)
		}
		grouped[r.KeyName] = append(grouped[r.KeyName], i)
	}

	indexes := make([]tableIndex, 0, len(order))
	for _, name := range order {
		ids := grouped[name]
		sort.Slice(ids, func(a, b int) bool {
			return rows[ids[a]].SeqInIndex < rows[ids[b]].SeqInIndex
		})
		idx := tableIndex{Name: name, Parts: make([]indexPart, len(ids))}
		for i, id := range ids {
			r := rows[id]
			part := indexPart{}
			if r.ColumnName.Valid {
				part.Column = r.ColumnName.String
			}
			if r.SubPart.Valid {
				part.SubPart = int(r.SubPart.Int64)
			}
			if r.Expression.Valid {
				part.Expression = r.Expression.String
			}
			idx.Parts[i] = part
		}
		indexes = append(indexes, idx)
	}
	return indexes, nil
}

func columnNames(columns []column) []string {
	names := make([]string, len(columns))
	for i, c := range columns {
		names[i] = c.ColumnName
	}
	return names
}
