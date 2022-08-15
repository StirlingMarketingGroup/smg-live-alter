package main

import "regexp"

var parseAlterRegexp = regexp.MustCompile("(?i)alter\\s+table\\s*(?:`?([^`]+)`?\\.)?`?([^`]+)`?\\s*([^;]+)")

var parseChangeColumnRegex = regexp.MustCompile("(?i)change\\s+column\\s*`?([^`]+)`?\\s+`?([^`]+)`?")
