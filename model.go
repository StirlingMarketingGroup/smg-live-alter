package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type columnPair struct {
	Source      string
	Destination string
}

// The copy and triggers share one ordered list so FIRST and AFTER cannot misalign source and destination values.
type columnMapping []columnPair

func buildColumnMapping(source []column, rename map[string]string, dest []column) (columnMapping, error) {
	destByName := make(map[string]column, len(dest))
	for _, c := range dest {
		destByName[c.ColumnName] = c
	}

	pairs := make(columnMapping, 0, len(source))
	for _, c := range source {
		destName, ok := rename[c.ColumnName]
		if !ok || destName == "" {
			destName = c.ColumnName
		}
		d, exists := destByName[destName]
		if c.GenerationExpression != "" {
			// Triggers cannot read a generated column, so a stored destination
			// would not receive the value the table will hold after the swap.
			if exists && d.GenerationExpression == "" {
				return nil, fmt.Errorf("column %q is generated on the source and stored on the destination; the triggers cannot read generated columns, so this alter can't be mirrored", c.ColumnName)
			}
			continue
		}
		if !exists || d.GenerationExpression != "" {
			continue
		}
		pairs = append(pairs, columnPair{Source: c.ColumnName, Destination: destName})
	}
	return pairs, nil
}

func (m columnMapping) insertList() string {
	names := make([]string, len(m))
	for i, p := range m {
		names[i] = p.Destination
	}
	return quoteIdents(names)
}

func (m columnMapping) selectList() string {
	names := make([]string, len(m))
	for i, p := range m {
		names[i] = p.Source
	}
	return quoteIdents(names)
}

func (m columnMapping) prefixedSources(prefix string) string {
	b := new(strings.Builder)
	for i, p := range m {
		if i != 0 {
			b.WriteByte(',')
		}
		b.WriteString(prefix)
		b.WriteString(quoteIdent(p.Source))
	}
	return b.String()
}

func (m columnMapping) assignments(prefix string) string {
	b := new(strings.Builder)
	for i, p := range m {
		if i != 0 {
			b.WriteByte(',')
		}
		b.WriteString(quoteIdent(p.Destination))
		b.WriteString("=")
		b.WriteString(prefix)
		b.WriteString(quoteIdent(p.Source))
	}
	return b.String()
}

func primaryKeyColumns(columns []column) []column {
	pks := make([]column, 0)
	for _, c := range columns {
		if c.PrimaryKey {
			pks = append(pks, c)
		}
	}
	sortKeyOrder(pks)
	return pks
}

func sortKeyOrder(columns []column) {
	sort.Slice(columns, func(i, j int) bool {
		return columns[i].SeqInIndex < columns[j].SeqInIndex
	})
}

// supportedTraversalKeyTypes are the DATA_TYPE values a user variable keeps
// comparable with the primary key. MySQL stores temporal values in user
// variables as strings, and it orders ENUM by member index, so those types
// (and floats) can't carry the cursor.
var supportedTraversalKeyTypes = map[string]struct{}{
	"tinyint":   {},
	"smallint":  {},
	"mediumint": {},
	"int":       {},
	"bigint":    {},
	"decimal":   {},
	"binary":    {},
	"varbinary": {},
	"char":      {},
	"varchar":   {},
}

func checkTraversalKeyTypes(key []column) error {
	if len(key) == 0 {
		return fmt.Errorf("table has no primary key; the copy pages by primary key")
	}
	for _, c := range key {
		if _, ok := supportedTraversalKeyTypes[strings.ToLower(c.DataType)]; !ok {
			return fmt.Errorf("primary key column %q has type %s; the copy cursor only supports tinyint, smallint, mediumint, int, bigint, decimal, binary, varbinary, char, varchar", c.ColumnName, c.DataType)
		}
	}
	return nil
}

type indexPart struct {
	Column     string
	SubPart    int
	Expression string
}

type tableIndex struct {
	Name  string
	Parts []indexPart
}

type projectionForm int

const (
	projectBare projectionForm = iota + 1
	projectWhenTrue
	projectWhenFalse
)

type conditionalProjection struct {
	Form       projectionForm
	Column     string
	CondColumn string
	Operator   string
	Number     string
}

var (
	bareProjectionRe = regexp.MustCompile("^`((?:[^`]|``)+)`$")
	ifColNullRe      = regexp.MustCompile("(?i)^if\\(\\(`((?:[^`]|``)+)` (<>|!=|<=|>=|=|<|>) (-?[0-9]+)\\),`((?:[^`]|``)+)`,null\\)$")
	ifNullColRe      = regexp.MustCompile("(?i)^if\\(\\(`((?:[^`]|``)+)` (<>|!=|<=|>=|=|<|>) (-?[0-9]+)\\),null,`((?:[^`]|``)+)`\\)$")
)

func parseConditionalProjection(expr string) (conditionalProjection, bool) {
	if m := bareProjectionRe.FindStringSubmatch(expr); m != nil {
		return conditionalProjection{Form: projectBare, Column: strings.ReplaceAll(m[1], "``", "`")}, true
	}
	if m := ifColNullRe.FindStringSubmatch(expr); m != nil {
		return conditionalProjection{
			Form:       projectWhenTrue,
			Column:     strings.ReplaceAll(m[4], "``", "`"),
			CondColumn: strings.ReplaceAll(m[1], "``", "`"),
			Operator:   m[2],
			Number:     m[3],
		}, true
	}
	if m := ifNullColRe.FindStringSubmatch(expr); m != nil {
		return conditionalProjection{
			Form:       projectWhenFalse,
			Column:     strings.ReplaceAll(m[4], "``", "`"),
			CondColumn: strings.ReplaceAll(m[1], "``", "`"),
			Operator:   m[2],
			Number:     m[3],
		}, true
	}
	return conditionalProjection{}, false
}

func integerDataType(dataType string) bool {
	switch strings.ToLower(dataType) {
	case "tinyint", "smallint", "mediumint", "int", "bigint":
		return true
	default:
		return false
	}
}

// An integer column compared with an integer literal evaluates the same under
// every session setting, and that column's type and value are unchanged, so
// the NULL pattern matches row for row. Where the value is not NULL, each
// side stores its projected column's value unchanged, and the copy keeps that
// value, so distinct source values stay distinct.
func derivedPairs(mapping columnMapping, rename map[string]string, source, dest []column) columnMapping {
	copied := make(map[string]string, len(mapping))
	for _, p := range mapping {
		copied[p.Source] = p.Destination
	}
	sourceByName := make(map[string]column, len(source))
	for _, c := range source {
		sourceByName[c.ColumnName] = c
	}
	destByName := make(map[string]column, len(dest))
	for _, c := range dest {
		destByName[c.ColumnName] = c
	}

	var pairs columnMapping
	for _, s := range source {
		if s.GenerationExpression == "" {
			continue
		}
		destName, ok := rename[s.ColumnName]
		if !ok || destName == "" {
			destName = s.ColumnName
		}
		d, ok := destByName[destName]
		if !ok || d.GenerationExpression == "" {
			continue
		}
		srcProj, srcOK := parseConditionalProjection(s.GenerationExpression)
		dstProj, dstOK := parseConditionalProjection(d.GenerationExpression)
		if !srcOK || !dstOK || srcProj.Form != dstProj.Form {
			continue
		}
		cDest, ok := copied[srcProj.Column]
		if !ok || dstProj.Column != cDest {
			continue
		}
		c, ok := sourceByName[srcProj.Column]
		if !ok {
			continue
		}
		cPrime, ok := destByName[cDest]
		if !ok {
			continue
		}
		if s.ColumnType != c.ColumnType || d.ColumnType != cPrime.ColumnType || s.CollationName != c.CollationName || d.CollationName != cPrime.CollationName {
			continue
		}
		if !keyConversionKeepsValues(c, cPrime) || c.CollationName != cPrime.CollationName {
			continue
		}
		if srcProj.Form != projectBare {
			fDest, ok := copied[srcProj.CondColumn]
			if !ok || dstProj.CondColumn != fDest || dstProj.Operator != srcProj.Operator || dstProj.Number != srcProj.Number {
				continue
			}
			f, ok := sourceByName[srcProj.CondColumn]
			if !ok || !integerDataType(f.DataType) {
				continue
			}
			fPrime, ok := destByName[fDest]
			if !ok || f.ColumnType != fPrime.ColumnType {
				continue
			}
		}
		pairs = append(pairs, columnPair{Source: s.ColumnName, Destination: destName})
	}
	return pairs
}

// A duplicate key during the copy is the same source row a trigger already
// wrote only when two source rows cannot land on one destination row.
// A derived column's values are proven equal to the source's, so its source
// index proves the same uniqueness.
func checkUniqueCoverage(mapping columnMapping, derived columnMapping, source, dest []column, sourceIndexes, destIndexes []tableIndex) error {
	destOf := make(map[string]string, len(mapping)+len(derived))
	sourceOf := make(map[string]string, len(mapping)+len(derived))
	for _, p := range mapping {
		destOf[p.Source] = p.Destination
		sourceOf[p.Destination] = p.Source
	}
	for _, p := range derived {
		destOf[p.Source] = p.Destination
		sourceOf[p.Destination] = p.Source
	}
	sourceByName := make(map[string]column, len(source))
	for _, c := range source {
		sourceByName[c.ColumnName] = c
	}
	destByName := make(map[string]column, len(dest))
	for _, c := range dest {
		destByName[c.ColumnName] = c
	}

	for _, d := range destIndexes {
		if !coveredBySource(d, sourceIndexes, destOf) {
			return fmt.Errorf("unique index %q on the destination is not covered by a unique index on the source, so the copy could merge two source rows into one", d.Name)
		}
		for _, part := range d.Parts {
			if part.Expression != "" || part.Column == "" {
				continue
			}
			srcName, ok := sourceOf[part.Column]
			if !ok {
				continue
			}
			srcCol := sourceByName[srcName]
			dstCol := destByName[part.Column]
			if srcCol.CollationName != dstCol.CollationName {
				return fmt.Errorf("unique index %q on the destination changes the collation of %q from %q to %q, so distinct source values can become equal", d.Name, part.Column, srcCol.CollationName, dstCol.CollationName)
			}
			if !keyConversionKeepsValues(srcCol, dstCol) {
				return fmt.Errorf("unique index %q on the destination changes %q from %s to %s; the copy and the triggers match rows by these values, so the conversion has to keep them", d.Name, part.Column, srcCol.ColumnType, dstCol.ColumnType)
			}
		}
	}
	return nil
}

func coveredBySource(dest tableIndex, sourceIndexes []tableIndex, destOf map[string]string) bool {
	for _, src := range sourceIndexes {
		if sourceIndexCovers(src, dest, destOf) {
			return true
		}
	}
	return false
}

func sourceIndexCovers(src, dest tableIndex, destOf map[string]string) bool {
	if len(src.Parts) == 0 {
		return false
	}
	for _, part := range src.Parts {
		// The same expression text can yield different values once its input
		// columns change type or charset, so a functional index covers nothing.
		if part.Expression != "" {
			return false
		}
		destCol, ok := destOf[part.Column]
		if !ok || !destHasColumnPart(dest, destCol, part.SubPart) {
			return false
		}
	}
	return true
}

func destHasColumnPart(dest tableIndex, column string, subPart int) bool {
	for _, part := range dest.Parts {
		if part.Expression != "" {
			continue
		}
		if part.Column == column && part.SubPart == subPart {
			return true
		}
	}
	return false
}

// destinationKey is the destination primary key in destination key order, each
// column paired with the source column copied into it. checkUniqueCoverage has
// proved that key is exactly one source row with its values kept, so the
// triggers find the temp row by it.
func destinationKey(mapping columnMapping, dest []column) (columnMapping, error) {
	sourceOf := make(map[string]string, len(mapping))
	for _, p := range mapping {
		sourceOf[p.Destination] = p.Source
	}

	destPK := primaryKeyColumns(dest)
	if len(destPK) == 0 {
		return nil, fmt.Errorf("the destination has no primary key")
	}
	key := make(columnMapping, 0, len(destPK))
	for _, c := range destPK {
		srcName, ok := sourceOf[c.ColumnName]
		if !ok {
			return nil, fmt.Errorf("primary key column %q on the destination is not copied from the source", c.ColumnName)
		}
		key = append(key, columnPair{Source: srcName, Destination: c.ColumnName})
	}
	return key, nil
}

// The copy's no-op duplicate and the triggers' key lookups compare the source
// value with the value the destination stored, so a unique column's conversion
// has to keep that value. Collation is checked beside this in
// checkUniqueCoverage.
func keyConversionKeepsValues(source, dest column) bool {
	if source.ColumnType == dest.ColumnType {
		return true
	}
	srcBase, srcLen, srcOK := splitColumnType(source.ColumnType)
	dstBase, dstLen, dstOK := splitColumnType(dest.ColumnType)
	if !srcOK || !dstOK {
		return false
	}
	if srcRank, srcInt := integerRank[srcBase]; srcInt {
		dstRank, dstInt := integerRank[dstBase]
		if !dstInt || columnUnsigned(source.ColumnType) != columnUnsigned(dest.ColumnType) {
			return false
		}
		return dstRank >= srcRank
	}
	if _, dstInt := integerRank[dstBase]; dstInt || srcLen <= 0 || dstLen < srcLen {
		return false
	}
	switch srcBase + "->" + dstBase {
	case "varchar->varchar", "varbinary->varbinary", "char->char", "char->varchar", "binary->varbinary":
		return true
	default:
		return false
	}
}

// tinyint < smallint < mediumint < int < bigint. A wider destination keeps
// every source integer; a narrower one does not.
var integerRank = map[string]int{
	"tinyint":   1,
	"smallint":  2,
	"mediumint": 3,
	"int":       4,
	"bigint":    5,
}

var columnTypeHead = regexp.MustCompile(`(?i)^\s*(varbinary|varchar|binary|tinyint|smallint|mediumint|bigint|char|int)\s*(?:\(\s*(\d+))?\s*(?:,\s*\d+\s*)?\)?`)

func splitColumnType(columnType string) (base string, length int, ok bool) {
	m := columnTypeHead.FindStringSubmatch(columnType)
	if m == nil {
		return "", 0, false
	}
	if m[2] != "" {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return "", 0, false
		}
		length = n
	}
	return strings.ToLower(m[1]), length, true
}

func columnUnsigned(columnType string) bool {
	return strings.Contains(strings.ToLower(columnType), "unsigned")
}
