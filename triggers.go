package main

import (
	"regexp"
	"strings"
)

var getNamesRegexp = regexp.MustCompile("`[^`]+?`")

func renameTriggerTable(sqlOriginalStatement string, newTableName string) string {
	firstLine, lastLines, _ := strings.Cut(sqlOriginalStatement, "\n")

	m := getNamesRegexp.FindAllStringIndex(firstLine, -1)
	last := m[len(m)-1]
	firstLine = firstLine[:last[0]+1] + newTableName + firstLine[last[1]-1:]

	return firstLine + "\n" + lastLines
}
