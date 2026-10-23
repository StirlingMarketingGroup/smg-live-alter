package main

import "testing"

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("a`b"); got != "`a``b`" {
		t.Errorf("got %s", got)
	}
}

func TestKeyAfterAndAtOrBefore(t *testing.T) {
	tests := []struct {
		cols       []string
		after      string
		atOrBefore string
	}{
		{
			cols:       []string{"a"},
			after:      "(`a`>@smgla_k0)",
			atOrBefore: "(`a`<=@smgla_e0)",
		},
		{
			cols:       []string{"a", "b"},
			after:      "((`a`>@smgla_k0) or (`a`=@smgla_k0 and `b`>@smgla_k1))",
			atOrBefore: "((`a`<@smgla_e0) or (`a`=@smgla_e0 and `b`<=@smgla_e1))",
		},
		{
			cols:       []string{"a", "b", "c"},
			after:      "((`a`>@smgla_k0) or (`a`=@smgla_k0 and `b`>@smgla_k1) or (`a`=@smgla_k0 and `b`=@smgla_k1 and `c`>@smgla_k2))",
			atOrBefore: "((`a`<@smgla_e0) or (`a`=@smgla_e0 and `b`<@smgla_e1) or (`a`=@smgla_e0 and `b`=@smgla_e1 and `c`<=@smgla_e2))",
		},
	}
	for _, tt := range tests {
		if got := keyAfter(tt.cols); got != tt.after {
			t.Errorf("keyAfter(%v) = %s, want %s", tt.cols, got, tt.after)
		}
		if got := keyAtOrBefore(tt.cols); got != tt.atOrBefore {
			t.Errorf("keyAtOrBefore(%v) = %s, want %s", tt.cols, got, tt.atOrBefore)
		}
	}
}
