package main

import (
	"reflect"
	"testing"
)

func TestSplitConstraints(t *testing.T) {
	create := "CREATE TABLE `emails` (\n" +
		"  `EmailID` binary(8) NOT NULL,\n" +
		"  `UserID` binary(8) DEFAULT NULL,\n" +
		"  PRIMARY KEY (`EmailID`),\n" +
		"  KEY `UserID` (`UserID`),\n" +
		"  CONSTRAINT `emails_users` FOREIGN KEY (`UserID`) REFERENCES `users` (`UserID`),\n" +
		"  CONSTRAINT `emails_chk_2` CHECK ((`UserID` is not null)),\n" +
		"  CONSTRAINT `has_user` CHECK ((`EmailID` <> `UserID`)),\n" +
		"  CONSTRAINT `emails_audit` FOREIGN KEY (`EmailID`) REFERENCES `audits` (`EmailID`)\n" +
		") ENGINE=InnoDB"

	fks, rest, err := splitConstraints(create, "emails", "emails_smgla_")
	if err != nil {
		t.Fatal(err)
	}

	wantFKs := []foreignKey{
		{Name: "emails_users", Definition: "FOREIGN KEY (`UserID`) REFERENCES `users` (`UserID`)"},
		{Name: "emails_audit", Definition: "FOREIGN KEY (`EmailID`) REFERENCES `audits` (`EmailID`)"},
	}
	if !reflect.DeepEqual(fks, wantFKs) {
		t.Errorf("foreign keys =\n%v\nwant\n%v", fks, wantFKs)
	}
	wantRest := "CREATE TABLE `emails` (\n" +
		"  `EmailID` binary(8) NOT NULL,\n" +
		"  `UserID` binary(8) DEFAULT NULL,\n" +
		"  PRIMARY KEY (`EmailID`),\n" +
		"  KEY `UserID` (`UserID`),\n" +
		"  CONSTRAINT `emails_smgla__chk_2` CHECK ((`UserID` is not null)),\n" +
		"  CHECK ((`EmailID` <> `UserID`))\n" +
		") ENGINE=InnoDB"
	if rest != wantRest {
		t.Errorf("rest =\n%s\nwant\n%s", rest, wantRest)
	}
}

func TestSplitConstraintsCheckLooksLikeForeignKey(t *testing.T) {
	create := "CREATE TABLE `t` (\n" +
		"  `c` varchar(20),\n" +
		"  CONSTRAINT `c_chk` CHECK (`c` = ' FOREIGN KEY ')\n" +
		") ENGINE=InnoDB"
	fks, rest, err := splitConstraints(create, "t", "t_smgla_")
	if err != nil {
		t.Fatal(err)
	}
	if len(fks) != 0 {
		t.Errorf("foreign keys = %v, want none", fks)
	}
	want := "CREATE TABLE `t` (\n" +
		"  `c` varchar(20),\n" +
		"  CHECK (`c` = ' FOREIGN KEY ')\n" +
		") ENGINE=InnoDB"
	if rest != want {
		t.Errorf("rest =\n%s\nwant\n%s", rest, want)
	}
}

func TestSplitConstraintsBacktickInName(t *testing.T) {
	create := "CREATE TABLE `t` (\n" +
		"  `p` int,\n" +
		"  CONSTRAINT `a``b` FOREIGN KEY (`p`) REFERENCES `parent` (`id`)\n" +
		") ENGINE=InnoDB"
	fks, _, err := splitConstraints(create, "t", "t_smgla_")
	if err != nil {
		t.Fatal(err)
	}
	want := []foreignKey{{Name: "a`b", Definition: "FOREIGN KEY (`p`) REFERENCES `parent` (`id`)"}}
	if len(fks) != 1 || fks[0] != want[0] {
		t.Fatalf("foreign keys = %#v, want %#v", fks, want)
	}
	if got := addForeignKeys("t_smgla_", fks, true); got != "alter table `t_smgla_` add constraint `a``b` FOREIGN KEY (`p`) REFERENCES `parent` (`id`)" {
		t.Errorf("named add: %s", got)
	}
	if got := addForeignKeys("probe", fks, false); got != "alter table `probe` add FOREIGN KEY (`p`) REFERENCES `parent` (`id`)" {
		t.Errorf("unnamed add: %s", got)
	}
}

func TestSplitConstraintsUnknownKind(t *testing.T) {
	create := "CREATE TABLE `t` (\n" +
		"  `id` int NOT NULL,\n" +
		"  CONSTRAINT `u` UNIQUE (`id`)\n" +
		") ENGINE=InnoDB"
	_, _, err := splitConstraints(create, "t", "t_smgla_")
	if err == nil || err.Error() != `constraint "u" is not a FOREIGN KEY or CHECK` {
		t.Fatalf("error %v", err)
	}
}

func TestRewriteCheckNames(t *testing.T) {
	rewrite := func(alter string) string {
		return rewriteCheckNames(alter, "t", "t_smgla_")
	}
	if got := rewrite("drop check t_chk_1"); got != "drop check t_smgla__chk_1" {
		t.Errorf("drop check: %s", got)
	}
	if got := rewrite("ALTER CHECK `t_chk_2` NOT ENFORCED"); got != "ALTER CHECK `t_smgla__chk_2` NOT ENFORCED" {
		t.Errorf("alter check: %s", got)
	}
	if got := rewrite("drop constraint t_chk_3"); got != "drop constraint t_smgla__chk_3" {
		t.Errorf("drop constraint: %s", got)
	}
	if got := rewrite("alter constraint `t_chk_4` enforced"); got != "alter constraint `t_smgla__chk_4` enforced" {
		t.Errorf("alter constraint: %s", got)
	}
	custom := "drop check has_user"
	if got := rewrite(custom); got != custom {
		t.Errorf("custom name changed to %s", got)
	}
	untouched := "add column t_chk_1 int default 't_chk_1'"
	if got := rewrite(untouched); got != untouched {
		t.Errorf("column and literal changed to %s", got)
	}
	inLiteral := "add column m varchar(100) default 'drop check t_chk_1'"
	if got := rewrite(inLiteral); got != inLiteral {
		t.Errorf("literal changed to %s", got)
	}
	quoteInName := "add column `it's` int, drop check t_chk_5"
	if got := rewrite(quoteInName); got != "add column `it's` int, drop check t_smgla__chk_5" {
		t.Errorf("quote inside a backtick name: %s", got)
	}
	inComment := "add column n int /* drop check t_chk_6 */, drop check t_chk_7"
	if got := rewrite(inComment); got != "add column n int /* drop check t_chk_6 */, drop check t_smgla__chk_7" {
		t.Errorf("comment: %s", got)
	}
	longer := "drop check t_chk_12a"
	if got := rewrite(longer); got != longer {
		t.Errorf("t_chk_12a changed to %s", got)
	}
	spaced := "drop\n  check\n\t`t_chk_9`"
	if got := rewrite(spaced); got != "drop\n  check\n\t`t_smgla__chk_9`" {
		t.Errorf("whitespace: %s", got)
	}
}

func TestReferentialActionSkipsTriggers(t *testing.T) {
	for _, rule := range []string{"CASCADE", "SET NULL", "SET DEFAULT", "cascade", "Set Null", "set default"} {
		if !referentialActionSkipsTriggers(rule) {
			t.Errorf("%s skips triggers", rule)
		}
	}
	for _, rule := range []string{"RESTRICT", "NO ACTION", "", "SET"} {
		if referentialActionSkipsTriggers(rule) {
			t.Errorf("%s fires triggers", rule)
		}
	}
}

func TestSplitConstraintsNone(t *testing.T) {
	create := "CREATE TABLE `t` (\n  `ID` int NOT NULL,\n  PRIMARY KEY (`ID`)\n) ENGINE=InnoDB"
	fks, rest, err := splitConstraints(create, "t", "t_smgla_")
	if err != nil {
		t.Fatal(err)
	}
	if len(fks) != 0 || rest != create {
		t.Errorf("got fks %v rest %q, want no change", fks, rest)
	}
}
