package main

import (
	"fmt"
	"strings"
)

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func quoteIdents(names []string) string {
	b := new(strings.Builder)
	for i, name := range names {
		if i != 0 {
			b.WriteByte(',')
		}
		b.WriteString(quoteIdent(name))
	}
	return b.String()
}

func quoteString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

// A strict lower bound prevents copying the preceding chunk again.
func keyAfter(columns []string) string {
	return lexPredicate(columns, "@smgla_k", ">", ">")
}

// An inclusive upper bound keeps the chunk-end row in this chunk.
func keyAtOrBefore(columns []string) string {
	return lexPredicate(columns, "@smgla_e", "<", "<=")
}

func lexPredicate(columns []string, varPrefix, cmp, lastCmp string) string {
	terms := make([]string, len(columns))
	for i := range columns {
		var b strings.Builder
		b.WriteByte('(')
		for j := 0; j < i; j++ {
			if j != 0 {
				b.WriteString(" and ")
			}
			fmt.Fprintf(&b, "%s=%s%d", quoteIdent(columns[j]), varPrefix, j)
		}
		if i != 0 {
			b.WriteString(" and ")
		}
		op := cmp
		if i == len(columns)-1 {
			op = lastCmp
		}
		fmt.Fprintf(&b, "%s%s%s%d)", quoteIdent(columns[i]), op, varPrefix, i)
		terms[i] = b.String()
	}
	if len(terms) == 1 {
		return terms[0]
	}
	return "(" + strings.Join(terms, " or ") + ")"
}

func sessionVarList(prefix string, n int) string {
	b := new(strings.Builder)
	for i := 0; i < n; i++ {
		if i != 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(b, "%s%d", prefix, i)
	}
	return b.String()
}

func cursorAssign(n int) string {
	b := new(strings.Builder)
	for i := 0; i < n; i++ {
		if i != 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "@smgla_k%d=@smgla_e%d", i, i)
	}
	return b.String()
}
