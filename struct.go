package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	dynamicstruct "github.com/Ompluscator/dynamic-struct"
	cool "github.com/StirlingMarketingGroup/cool-mysql"
)

func tableRowStruct(columns []*column) (bld dynamicstruct.Builder, pkIndexes []int, err error) {
	// this is our dynamic struct of the actual row, which will have
	// properties added to it for each column in the following loop
	rowStruct := dynamicstruct.NewStruct()

	i := 0
	for _, c := range columns {
		// you can't insert into generated columns, and mysql will actually
		// throw errors if you try and do this, so we simply skip them altogether
		// and imagine they don't exist
		if len(c.GenerationExpression) != 0 {
			continue
		}

		// column type will end with "unsigned" if the unsigned flag is set for
		// this column, used for unsigned integers
		unsigned := strings.HasSuffix(c.ColumnType, "unsigned")

		// these are our struct fields, which all look like "F0", "F1", etc
		f := "F" + strconv.Itoa(c.Position)

		// create the tag for the field with the exact column name so that
		// cool mysql insert func knows how to map the row values
		tag := `mysql:"` + c.ColumnName + `,omitempty"`

		var v interface{}

		// the switch through data types (different than column types, doesn't include lengths)
		// to determine the type of our struct field
		// All of the field types are pointers so that our mysql scanning
		// handles null values gracefully
		switch c.DataType {
		case "tinyint":
			if unsigned {
				v = new(uint8)
			} else {
				v = new(int8)
			}
		case "smallint":
			if unsigned {
				v = new(uint16)
			} else {
				v = new(int16)
			}
		case "int", "mediumint":
			if unsigned {
				v = new(uint32)
			} else {
				v = new(int32)
			}
		case "bigint":
			if unsigned {
				v = new(uint64)
			} else {
				v = new(int64)
			}
		case "float":
			v = new(float64)
		case "decimal", "double":
			// our cool mysql literal is exactly what it sounds like;
			// passed directly into the query with no escaping, which is know is
			// safe here because a decimal from mysql can't contain breaking characters
			v = new(cool.RawMySQL)
		case "timestamp", "date", "datetime":
			v = new(string)
		case "binary", "varbinary", "blob", "tinyblob", "mediumblob", "longblob":
			v = new([]byte)
		case "char", "varchar", "text", "tinytext", "mediumtext", "longtext", "enum":
			v = new(string)
		case "json":
			// the json type here is important, because mysql needs
			// char set info for json columns, since json is supposed to be utf8,
			// and go treats this is bytes for some reason. json.RawMessage lets cool mysql
			// know to surround the inlined value with charset info
			v = new(json.RawMessage)
		case "set":
			v = new(any)
		default:
			return nil, nil, fmt.Errorf("unknown mysql column of type %q", c.ColumnType)
		}

		rowStruct.AddField(f, v, tag)
		if c.PrimaryKey {
			pkIndexes = append(pkIndexes, i)
		}
		i++
	}

	return rowStruct, pkIndexes, nil
}
