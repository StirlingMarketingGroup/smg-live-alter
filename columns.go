package main

import (
	"strings"

	cool "github.com/StirlingMarketingGroup/cool-mysql"
)

// the mysql lib we're using, cool mysql, lets us define a
// chan of structs to read our rows into, for awesome
// performance and type safety. The tags are the actual column names
type column struct {
	ColumnName           string `mysql:"COLUMN_NAME"`
	Position             int    `mysql:"ORDINAL_POSITION"`
	DataType             string `mysql:"DATA_TYPE"`
	ColumnType           string `mysql:"COLUMN_TYPE"`
	GenerationExpression string `mysql:"GENERATION_EXPRESSION"`
	PrimaryKey           bool
}

func getTableColumns(db *cool.Database, tableName string) ([]*column, error) {
	var columns []*column

	// we need to check to see if the db supports generated columns
	// if it doesn't, our query to get column info will fail
	columnInfoCols := "`COLUMN_NAME`,`ORDINAL_POSITION`,`DATA_TYPE`,`COLUMN_TYPE`"
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

	// in this query we're simply getting all the details about our column names
	// so we can make a dynamic struct that the rows can fit into
	err = db.Select(&columns, "select"+columnInfoCols+
		"from`INFORMATION_SCHEMA`.`columns`"+
		"where`TABLE_SCHEMA`=database()"+
		"and`table_name`='"+tableName+"'"+
		"order by`ORDINAL_POSITION`", 0)
	if err != nil {
		return nil, err
	}

	var primaryKeys []*struct {
		ColumnName string `mysql:"Column_name"`
	}
	err = db.Select(&primaryKeys, "show index from`"+tableName+"`where`Key_name`='PRIMARY'", 0)
	if err != nil {
		return nil, err
	}

	primaryKeysSet := make(map[string]struct{}, len(primaryKeys))
	for _, pk := range primaryKeys {
		primaryKeysSet[pk.ColumnName] = struct{}{}
	}

	for _, c := range columns {
		if _, ok := primaryKeysSet[c.ColumnName]; ok {
			c.PrimaryKey = true
		}
	}

	return columns, nil
}

func quoteColumns(columns []*column) string {
	return quoteColumnsPrefix(columns, "")
}

func quoteColumnsPrefix(columns []*column, prefix string) string {
	// this is our string builder for quoted column names,
	// which will be used in our select statement
	columnsQuotedBld := new(strings.Builder)

	for i, c := range columns {
		// column string should be like "`Column1`,`Column2`..."
		if i != 0 {
			columnsQuotedBld.WriteByte(',')
		}
		columnsQuotedBld.WriteString(prefix)
		columnsQuotedBld.WriteByte('`')
		columnsQuotedBld.WriteString(c.ColumnName)
		columnsQuotedBld.WriteByte('`')
	}

	return columnsQuotedBld.String()
}

func columnsSet(columns []*column) map[string]struct{} {
	set := make(map[string]struct{}, len(columns))
	for _, c := range columns {
		set[c.ColumnName] = struct{}{}
	}
	return set
}
