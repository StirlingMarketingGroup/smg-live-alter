package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuildColumnMapping(t *testing.T) {
	tests := []struct {
		name         string
		source, dest []column
		rename       map[string]string
		want         columnMapping
		wantInsert   string
		wantSelect   string
	}{
		{
			name:   "reordered destination",
			source: []column{{ColumnName: "a"}, {ColumnName: "b"}, {ColumnName: "c"}},
			// Source order keeps the insert and select lists aligned when the destination column order differs.
			dest:       []column{{ColumnName: "c"}, {ColumnName: "a"}, {ColumnName: "b"}},
			want:       columnMapping{{"a", "a"}, {"b", "b"}, {"c", "c"}},
			wantInsert: "`a`,`b`,`c`",
			wantSelect: "`a`,`b`,`c`",
		},
		{
			name:       "renamed column",
			source:     []column{{ColumnName: "old_name"}, {ColumnName: "b"}},
			dest:       []column{{ColumnName: "b"}, {ColumnName: "new_name"}},
			rename:     map[string]string{"old_name": "new_name"},
			want:       columnMapping{{"old_name", "new_name"}, {"b", "b"}},
			wantInsert: "`new_name`,`b`",
			wantSelect: "`old_name`,`b`",
		},
		{
			name:       "dropped column",
			source:     []column{{ColumnName: "a"}, {ColumnName: "gone"}, {ColumnName: "b"}},
			dest:       []column{{ColumnName: "a"}, {ColumnName: "b"}},
			want:       columnMapping{{"a", "a"}, {"b", "b"}},
			wantInsert: "`a`,`b`",
			wantSelect: "`a`,`b`",
		},
		{
			name:       "added column",
			source:     []column{{ColumnName: "a"}},
			dest:       []column{{ColumnName: "a"}, {ColumnName: "extra"}},
			want:       columnMapping{{"a", "a"}},
			wantInsert: "`a`",
			wantSelect: "`a`",
		},
		{
			name: "generated columns on either side",
			source: []column{
				{ColumnName: "id"},
				{ColumnName: "src_gen", GenerationExpression: "`id`"},
				{ColumnName: "becomes_gen"},
				{ColumnName: "kept"},
			},
			dest: []column{
				{ColumnName: "kept"},
				{ColumnName: "id"},
				{ColumnName: "becomes_gen", GenerationExpression: "`id`"},
				{ColumnName: "dst_gen", GenerationExpression: "`id`"},
			},
			want:       columnMapping{{"id", "id"}, {"kept", "kept"}},
			wantInsert: "`id`,`kept`",
			wantSelect: "`id`,`kept`",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rename := tt.rename
			if rename == nil {
				rename = map[string]string{}
				for _, c := range tt.source {
					rename[c.ColumnName] = c.ColumnName
				}
			} else {
				for _, c := range tt.source {
					if _, ok := rename[c.ColumnName]; !ok {
						rename[c.ColumnName] = c.ColumnName
					}
				}
			}
			got, err := buildColumnMapping(tt.source, rename, tt.dest)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("pairs = %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("pair %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
			if got.insertList() != tt.wantInsert || got.selectList() != tt.wantSelect {
				t.Errorf("lists insert %s select %s, want insert %s select %s", got.insertList(), got.selectList(), tt.wantInsert, tt.wantSelect)
			}
		})
	}
}

func TestPrimaryKeyColumnsOrder(t *testing.T) {
	columns := []column{
		{ColumnName: "b", PrimaryKey: true, SeqInIndex: 2},
		{ColumnName: "other"},
		{ColumnName: "a", PrimaryKey: true, SeqInIndex: 1},
	}
	got := primaryKeyColumns(columns)
	if len(got) != 2 || got[0].ColumnName != "a" || got[1].ColumnName != "b" {
		t.Fatalf("primary key order = %+v, want a then b", got)
	}
}

func TestTraversalKeyTypes(t *testing.T) {
	accepted := []string{"tinyint", "smallint", "mediumint", "int", "bigint", "decimal", "binary", "varbinary", "char", "varchar"}
	rejected := []string{"datetime", "timestamp", "date", "enum", "set", "float", "double"}

	for _, dt := range accepted {
		key := []column{{ColumnName: "id", DataType: dt, PrimaryKey: true, SeqInIndex: 1}}
		if err := checkTraversalKeyTypes(key); err != nil {
			t.Errorf("%s: %v", dt, err)
		}
	}
	for _, dt := range rejected {
		key := []column{
			{ColumnName: "id", DataType: "int", PrimaryKey: true, SeqInIndex: 1},
			{ColumnName: "when", DataType: dt, PrimaryKey: true, SeqInIndex: 2},
		}
		err := checkTraversalKeyTypes(key)
		if err == nil {
			t.Errorf("%s: expected an error", dt)
			continue
		}
		if !strings.Contains(err.Error(), dt) {
			t.Errorf("%s: error %q does not name the type", dt, err)
		}
	}
}

func TestBuildColumnMappingGeneratedStored(t *testing.T) {
	_, err := buildColumnMapping(
		[]column{{ColumnName: "g", GenerationExpression: "(`x` * 2)"}},
		map[string]string{"g": "g"},
		[]column{{ColumnName: "g"}},
	)
	if err == nil || !strings.Contains(err.Error(), `column "g" is generated on the source and stored on the destination`) {
		t.Fatalf("error %v", err)
	}

	got, err := buildColumnMapping(
		[]column{{ColumnName: "g", GenerationExpression: "(`x` * 2)"}, {ColumnName: "id"}},
		map[string]string{"g": "g", "id": "id"},
		[]column{{ColumnName: "g", GenerationExpression: "(`x` * 2)"}, {ColumnName: "id"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (columnPair{Source: "id", Destination: "id"}) {
		t.Fatalf("pairs = %+v", got)
	}

	got, err = buildColumnMapping(
		[]column{{ColumnName: "g", GenerationExpression: "1"}, {ColumnName: "id"}},
		map[string]string{"g": "g", "id": "id"},
		[]column{{ColumnName: "id"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Source != "id" {
		t.Fatalf("pairs = %+v", got)
	}
}

func pairs(ps ...string) columnMapping {
	m := make(columnMapping, 0, len(ps)/2)
	for i := 0; i < len(ps); i += 2 {
		m = append(m, columnPair{Source: ps[i], Destination: ps[i+1]})
	}
	return m
}

func colPart(column string, sub int) indexPart {
	return indexPart{Column: column, SubPart: sub}
}

func exprPart(expression string) indexPart {
	return indexPart{Expression: expression}
}

func uindex(name string, parts ...indexPart) tableIndex {
	return tableIndex{Name: name, Parts: parts}
}

func named(names ...string) []column {
	cols := make([]column, len(names))
	for i, name := range names {
		cols[i] = column{ColumnName: name}
	}
	return cols
}

func TestParseConditionalProjection(t *testing.T) {
	tests := []struct {
		expr string
		ok   bool
		want conditionalProjection
	}{
		{
			expr: "if((`__Latest` <> 0),`EmailID`,NULL)",
			ok:   true,
			want: conditionalProjection{Form: projectWhenTrue, Column: "EmailID", CondColumn: "__Latest", Operator: "<>", Number: "0"},
		},
		{
			expr: "IF((`__Expired` = 0),`EmailID`,null)",
			ok:   true,
			want: conditionalProjection{Form: projectWhenTrue, Column: "EmailID", CondColumn: "__Expired", Operator: "=", Number: "0"},
		},
		{
			expr: "if((`n` >= -3),NULL,`c`)",
			ok:   true,
			want: conditionalProjection{Form: projectWhenFalse, Column: "c", CondColumn: "n", Operator: ">=", Number: "-3"},
		},
		{
			expr: "`c`",
			ok:   true,
			want: conditionalProjection{Form: projectBare, Column: "c"},
		},
		{expr: "if((char_length(`flag`) = 1),`c`,NULL)"},
		{expr: "if((`a` = _utf8mb4'x'),`c`,NULL)"},
		{expr: "if(((`a` = 0) and (`b` = 0)),`c`,NULL)"},
		{expr: "if((`a` is null),`c`,NULL)"},
		{expr: "if((`a` = `b`),`c`,NULL)"},
		{expr: "if((`a` = 0),`b`,`c`)"},
		{expr: "concat(`c`,'x')"},
		{expr: "(`c` + 1)"},
		{expr: "_binary'a'"},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			got, ok := parseConditionalProjection(tt.expr)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v (%+v)", ok, tt.ok, got)
			}
			if ok && got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDerivedPairs(t *testing.T) {
	col := func(name, dataType, columnType, expr string) column {
		return column{ColumnName: name, DataType: dataType, ColumnType: columnType, GenerationExpression: expr}
	}
	latest := "if((`__Latest` <> 0),`EmailID`,NULL)"
	tests := []struct {
		name         string
		mapping      columnMapping
		rename       map[string]string
		source, dest []column
		want         columnMapping
	}{
		{
			name:    "versioned latest column",
			mapping: pairs("EmailID", "EmailID", "__Latest", "__Latest"),
			source: []column{
				col("EmailID", "binary", "binary(8)", ""),
				col("__Latest", "tinyint", "tinyint", ""),
				col("$SingleLatestPer", "binary", "binary(8)", latest),
			},
			dest: []column{
				col("EmailID", "varbinary", "varbinary(12)", ""),
				col("__Latest", "tinyint", "tinyint", ""),
				col("$SingleLatestPer", "varbinary", "varbinary(12)", latest),
			},
			want: pairs("$SingleLatestPer", "$SingleLatestPer"),
		},
		{
			name:    "projected column renamed",
			mapping: pairs("EmailID", "ID", "__Latest", "__Latest"),
			rename:  map[string]string{"EmailID": "ID"},
			source: []column{
				col("EmailID", "binary", "binary(8)", ""),
				col("__Latest", "tinyint", "tinyint", ""),
				col("$SingleLatestPer", "binary", "binary(8)", latest),
			},
			dest: []column{
				col("ID", "varbinary", "varbinary(12)", ""),
				col("__Latest", "tinyint", "tinyint", ""),
				col("$SingleLatestPer", "varbinary", "varbinary(12)", "if((`__Latest` <> 0),`ID`,NULL)"),
			},
			want: pairs("$SingleLatestPer", "$SingleLatestPer"),
		},
		{
			name:    "condition column renamed",
			mapping: pairs("EmailID", "EmailID", "__Latest", "L"),
			rename:  map[string]string{"__Latest": "L"},
			source: []column{
				col("EmailID", "binary", "binary(8)", ""),
				col("__Latest", "tinyint", "tinyint", ""),
				col("$SingleLatestPer", "binary", "binary(8)", latest),
			},
			dest: []column{
				col("EmailID", "varbinary", "varbinary(12)", ""),
				col("L", "tinyint", "tinyint", ""),
				col("$SingleLatestPer", "varbinary", "varbinary(12)", "if((`L` <> 0),`EmailID`,NULL)"),
			},
			want: pairs("$SingleLatestPer", "$SingleLatestPer"),
		},
		{
			name:    "destination condition differs",
			mapping: pairs("EmailID", "EmailID", "__Latest", "__Latest"),
			source: []column{
				col("EmailID", "binary", "binary(8)", ""),
				col("__Latest", "tinyint", "tinyint", ""),
				col("$SingleLatestPer", "binary", "binary(8)", latest),
			},
			dest: []column{
				col("EmailID", "varbinary", "varbinary(12)", ""),
				col("__Latest", "tinyint", "tinyint", ""),
				col("$SingleLatestPer", "varbinary", "varbinary(12)", "if((`__Expired` <> 0),`EmailID`,NULL)"),
			},
		},
		{
			name:    "condition column type changes",
			mapping: pairs("EmailID", "EmailID", "__Latest", "__Latest"),
			source: []column{
				col("EmailID", "binary", "binary(8)", ""),
				col("__Latest", "tinyint", "tinyint", ""),
				col("$SingleLatestPer", "binary", "binary(8)", latest),
			},
			dest: []column{
				col("EmailID", "varbinary", "varbinary(12)", ""),
				col("__Latest", "int", "int", ""),
				col("$SingleLatestPer", "varbinary", "varbinary(12)", latest),
			},
		},
		{
			name:    "condition column is not an integer",
			mapping: pairs("flag", "flag", "c", "c"),
			source: []column{
				col("flag", "char", "char(2)", ""),
				col("c", "binary", "binary(8)", ""),
				col("g", "binary", "binary(8)", "if((`flag` = 1),`c`,NULL)"),
			},
			dest: []column{
				col("flag", "char", "char(2)", ""),
				col("c", "binary", "binary(8)", ""),
				col("g", "binary", "binary(8)", "if((`flag` = 1),`c`,NULL)"),
			},
		},
		{
			name:    "function call in the condition",
			mapping: pairs("flag", "flag", "c", "c"),
			source: []column{
				col("flag", "char", "char(2)", ""),
				col("c", "binary", "binary(8)", ""),
				col("g", "binary", "binary(8)", "if((char_length(`flag`) = 1),`c`,NULL)"),
			},
			dest: []column{
				col("flag", "char", "char(2)", ""),
				col("c", "binary", "binary(8)", ""),
				col("g", "binary", "binary(8)", "if((char_length(`flag`) = 1),`c`,NULL)"),
			},
		},
		{
			name:    "projected conversion does not keep values",
			mapping: pairs("flag", "flag", "c", "c"),
			source: []column{
				col("flag", "tinyint", "tinyint", ""),
				col("c", "varbinary", "varbinary(2)", ""),
				col("g", "varbinary", "varbinary(2)", "if((`flag` = 1),`c`,NULL)"),
			},
			dest: []column{
				col("flag", "tinyint", "tinyint", ""),
				col("c", "binary", "binary(2)", ""),
				col("g", "binary", "binary(2)", "if((`flag` = 1),`c`,NULL)"),
			},
		},
		{
			name:    "generated type differs from the projected column",
			mapping: pairs("EmailID", "EmailID", "__Latest", "__Latest"),
			source: []column{
				col("EmailID", "binary", "binary(8)", ""),
				col("__Latest", "tinyint", "tinyint", ""),
				col("$SingleLatestPer", "binary", "binary(8)", latest),
			},
			dest: []column{
				col("EmailID", "varbinary", "varbinary(12)", ""),
				col("__Latest", "tinyint", "tinyint", ""),
				col("$SingleLatestPer", "varbinary", "varbinary(16)", latest),
			},
		},
		{
			name:    "literal renamed inside the condition",
			mapping: pairs("a", "b", "flag", "flag"),
			rename:  map[string]string{"a": "b"},
			source: []column{
				col("flag", "tinyint", "tinyint", ""),
				col("a", "binary", "binary(8)", ""),
				col("g", "binary", "binary(8)", "if((`flag` = '`a`'),`a`,NULL)"),
			},
			dest: []column{
				col("flag", "tinyint", "tinyint", ""),
				col("b", "binary", "binary(8)", ""),
				col("g", "binary", "binary(8)", "if((`flag` = '`b`'),`b`,NULL)"),
			},
		},
		{
			name:   "constant expression",
			source: []column{col("g", "binary", "binary(1)", "_binary'a'")},
			dest:   []column{col("g", "binary", "binary(1)", "_binary'a'")},
		},
		{
			name: "projected column is generated",
			source: []column{
				col("inner", "binary", "binary(8)", "`c`"),
				col("g", "binary", "binary(8)", "`inner`"),
			},
			dest: []column{
				col("inner", "binary", "binary(8)", "`c`"),
				col("g", "binary", "binary(8)", "`inner`"),
			},
		},
		{
			name:    "destination column is stored",
			mapping: pairs("c", "c"),
			source: []column{
				col("c", "binary", "binary(8)", ""),
				col("g", "binary", "binary(8)", "`c`"),
			},
			dest: []column{
				col("c", "binary", "binary(8)", ""),
				col("g", "binary", "binary(8)", ""),
			},
		},
		{
			name:    "bare projection",
			mapping: pairs("c", "c"),
			source: []column{
				col("c", "binary", "binary(8)", ""),
				col("g", "binary", "binary(8)", "`c`"),
			},
			dest: []column{
				col("c", "varbinary", "varbinary(12)", ""),
				col("g", "varbinary", "varbinary(12)", "`c`"),
			},
			want: pairs("g", "g"),
		},
		{
			name:    "backtick in the projected column name",
			mapping: pairs("a`b", "a`b"),
			source: []column{
				col("a`b", "binary", "binary(8)", ""),
				col("g", "binary", "binary(8)", "`a``b`"),
			},
			dest: []column{
				col("a`b", "varbinary", "varbinary(12)", ""),
				col("g", "varbinary", "varbinary(12)", "`a``b`"),
			},
			want: pairs("g", "g"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := derivedPairs(tt.mapping, tt.rename, tt.source, tt.dest)
			if len(got) != len(tt.want) {
				t.Fatalf("pairs = %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("pairs = %+v, want %+v", got, tt.want)
				}
			}
		})
	}
}

func TestCheckUniqueCoverage(t *testing.T) {
	same := pairs("a", "a", "b", "b", "email", "email", "id", "id")
	cols := named("a", "b", "email", "id")
	tests := []struct {
		name                       string
		mapping                    columnMapping
		derived                    columnMapping
		source, dest               []column
		sourceIndexes, destIndexes []tableIndex
		wantErr                    string
	}{
		{
			name:          "same pk",
			mapping:       pairs("a", "a"),
			source:        named("a"),
			dest:          named("a"),
			sourceIndexes: []tableIndex{uindex("PRIMARY", colPart("a", 0))},
			destIndexes:   []tableIndex{uindex("PRIMARY", colPart("a", 0))},
		},
		{
			name:          "pk grown",
			mapping:       pairs("a", "a", "b", "b"),
			source:        named("a", "b"),
			dest:          named("a", "b"),
			sourceIndexes: []tableIndex{uindex("PRIMARY", colPart("a", 0))},
			destIndexes:   []tableIndex{uindex("PRIMARY", colPart("a", 0), colPart("b", 0))},
		},
		{
			name:          "pk shrunk",
			mapping:       pairs("a", "a", "b", "b"),
			source:        named("a", "b"),
			dest:          named("a", "b"),
			sourceIndexes: []tableIndex{uindex("PRIMARY", colPart("a", 0), colPart("b", 0))},
			destIndexes:   []tableIndex{uindex("PRIMARY", colPart("a", 0))},
			wantErr:       "not covered",
		},
		{
			name:    "pk shrunk covered by unique",
			mapping: pairs("a", "a", "b", "b"),
			source:  named("a", "b"),
			dest:    named("a", "b"),
			sourceIndexes: []tableIndex{
				uindex("PRIMARY", colPart("a", 0), colPart("b", 0)),
				uindex("uq_a", colPart("a", 0)),
			},
			destIndexes: []tableIndex{uindex("PRIMARY", colPart("a", 0))},
		},
		{
			name:          "added unique on a non-unique column",
			mapping:       pairs("id", "id", "email", "email"),
			source:        named("id", "email"),
			dest:          named("id", "email"),
			sourceIndexes: []tableIndex{uindex("PRIMARY", colPart("id", 0))},
			destIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_email", colPart("email", 0)),
			},
			wantErr: "uq_email",
		},
		{
			name:    "unique kept",
			mapping: pairs("id", "id", "email", "email"),
			source:  named("id", "email"),
			dest:    named("id", "email"),
			sourceIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_email", colPart("email", 0)),
			},
			destIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_email", colPart("email", 0)),
			},
		},
		{
			name:          "renamed key column",
			mapping:       pairs("a", "b"),
			source:        named("a"),
			dest:          named("b"),
			sourceIndexes: []tableIndex{uindex("PRIMARY", colPart("a", 0))},
			destIndexes:   []tableIndex{uindex("PRIMARY", colPart("b", 0))},
		},
		{
			name:    "prefix length change",
			mapping: pairs("id", "id", "email", "email"),
			source:  named("id", "email"),
			dest:    named("id", "email"),
			sourceIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_email", colPart("email", 10)),
			},
			destIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_email", colPart("email", 20)),
			},
			wantErr: "not covered",
		},
		{
			name:    "collation change on a unique column",
			mapping: pairs("id", "id", "email", "email"),
			source:  []column{{ColumnName: "id"}, {ColumnName: "email", CollationName: "utf8mb4_general_ci"}},
			dest:    []column{{ColumnName: "id"}, {ColumnName: "email", CollationName: "utf8mb4_bin"}},
			sourceIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_email", colPart("email", 0)),
			},
			destIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_email", colPart("email", 0)),
			},
			wantErr: "collation",
		},
		{
			name:    "identical functional unique part",
			mapping: same,
			source:  cols,
			dest:    cols,
			sourceIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_email", colPart("email", 0)),
				uindex("uq_fn", exprPart("(lower(`email`))")),
			},
			destIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_fn", colPart("email", 0), exprPart("(lower(`email`))")),
			},
		},
		{
			name:    "source functional index does not cover the same expression",
			mapping: pairs("id", "id", "email", "email"),
			source:  named("id", "email"),
			dest:    named("id", "email"),
			sourceIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_fn", exprPart("(lower(`email`))")),
			},
			destIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_fn", exprPart("(lower(`email`))")),
			},
			wantErr: "uq_fn",
		},
		{
			name:    "varbinary to binary unique",
			mapping: pairs("id", "id", "k", "k"),
			source: []column{
				{ColumnName: "id", ColumnType: "int"},
				{ColumnName: "k", ColumnType: "varbinary(2)"},
			},
			dest: []column{
				{ColumnName: "id", ColumnType: "int"},
				{ColumnName: "k", ColumnType: "binary(2)"},
			},
			sourceIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_k", colPart("k", 0)),
			},
			destIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_k", colPart("k", 0)),
			},
			wantErr: `unique index "uq_k" on the destination changes "k" from varbinary(2) to binary(2); the copy and the triggers match rows by these values, so the conversion has to keep them`,
		},
		{
			name:    "padded binary primary key",
			mapping: pairs("k", "k"),
			source:  []column{{ColumnName: "k", ColumnType: "binary(1)"}},
			dest:    []column{{ColumnName: "k", ColumnType: "binary(2)"}},
			sourceIndexes: []tableIndex{
				uindex("PRIMARY", colPart("k", 0)),
			},
			destIndexes: []tableIndex{
				uindex("PRIMARY", colPart("k", 0)),
			},
			wantErr: `unique index "PRIMARY" on the destination changes "k" from binary(1) to binary(2)`,
		},
		{
			name:    "derived generated unique",
			mapping: pairs("EmailID", "EmailID"),
			derived: pairs("$SingleLatestPer", "$SingleLatestPer"),
			source: []column{
				{ColumnName: "EmailID", ColumnType: "binary(8)"},
				{ColumnName: "$SingleLatestPer", ColumnType: "binary(8)"},
			},
			dest: []column{
				{ColumnName: "EmailID", ColumnType: "varbinary(12)"},
				{ColumnName: "$SingleLatestPer", ColumnType: "varbinary(12)"},
			},
			sourceIndexes: []tableIndex{uindex("$SingleLatestPer", colPart("$SingleLatestPer", 0))},
			destIndexes:   []tableIndex{uindex("$SingleLatestPer", colPart("$SingleLatestPer", 0))},
		},
		{
			name:    "generated unique without a derived pair",
			mapping: pairs("EmailID", "EmailID"),
			source: []column{
				{ColumnName: "EmailID", ColumnType: "binary(8)"},
				{ColumnName: "$SingleLatestPer", ColumnType: "binary(8)"},
			},
			dest: []column{
				{ColumnName: "EmailID", ColumnType: "varbinary(12)"},
				{ColumnName: "$SingleLatestPer", ColumnType: "varbinary(12)"},
			},
			sourceIndexes: []tableIndex{uindex("$SingleLatestPer", colPart("$SingleLatestPer", 0))},
			destIndexes:   []tableIndex{uindex("$SingleLatestPer", colPart("$SingleLatestPer", 0))},
			wantErr:       "not covered",
		},
		{
			name:    "binary to varbinary unique",
			mapping: pairs("id", "id", "k", "k"),
			source: []column{
				{ColumnName: "id", ColumnType: "int"},
				{ColumnName: "k", ColumnType: "binary(8)"},
			},
			dest: []column{
				{ColumnName: "id", ColumnType: "int"},
				{ColumnName: "k", ColumnType: "varbinary(12)"},
			},
			sourceIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_k", colPart("k", 0)),
			},
			destIndexes: []tableIndex{
				uindex("PRIMARY", colPart("id", 0)),
				uindex("uq_k", colPart("k", 0)),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkUniqueCoverage(tt.mapping, tt.derived, tt.source, tt.dest, tt.sourceIndexes, tt.destIndexes)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestDestinationKey(t *testing.T) {
	pk := func(name, columnType string, seq int) column {
		return column{ColumnName: name, ColumnType: columnType, PrimaryKey: true, SeqInIndex: seq}
	}
	stored := func(name, columnType string) column {
		return column{ColumnName: name, ColumnType: columnType}
	}

	got, err := destinationKey(
		pairs("a", "a", "b", "b"),
		[]column{pk("b", "int", 1), pk("a", "int", 2)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(pairs("b", "b", "a", "a")) {
		t.Fatalf("order = %v, want destination SeqInIndex b then a", got)
	}

	got, err = destinationKey(
		pairs("old_name", "new_name"),
		[]column{pk("new_name", "int", 1)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(pairs("old_name", "new_name")) {
		t.Fatalf("renamed = %v", got)
	}

	_, err = destinationKey(
		pairs("a", "a"),
		[]column{pk("a", "int", 1), pk("added", "int", 2)},
	)
	if err == nil || !strings.Contains(err.Error(), `primary key column "added" on the destination is not copied from the source`) {
		t.Fatalf("error %v", err)
	}

	_, err = destinationKey(pairs("a", "a"), []column{stored("a", "int")})
	if err == nil || !strings.Contains(err.Error(), "the destination has no primary key") {
		t.Fatalf("no destination key: error %v", err)
	}

}

func TestKeyConversionKeepsValues(t *testing.T) {
	cases := []struct {
		src, dst string
		want     bool
	}{
		{"binary(8)", "varbinary(12)", true},
		{"int", "bigint", true},
		{"bigint", "int", false},
		{"int", "int unsigned", false},
		{"varchar(10)", "varchar(20)", true},
		{"varchar(20)", "varchar(10)", false},
		{"binary(1)", "binary(2)", false},
		{"char(5)", "varchar(5)", true},
		{"decimal(10,2)", "decimal(10,2)", true},
		{"decimal(10,2)", "decimal(12,2)", false},
	}
	for _, tc := range cases {
		got := keyConversionKeepsValues(column{ColumnType: tc.src}, column{ColumnType: tc.dst})
		if got != tc.want {
			t.Errorf("%s → %s = %v, want %v", tc.src, tc.dst, got, tc.want)
		}
	}
}
