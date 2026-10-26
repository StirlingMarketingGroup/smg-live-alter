package main

import (
	"strings"
	"testing"
)

func TestRestorePlanSteps(t *testing.T) {
	p := restorePlan{
		ForeignKeys: []foreignKey{
			{Name: "fk1", Definition: "FOREIGN KEY (`a`) REFERENCES `p` (`id`)"},
			{Name: "fk2", Definition: "FOREIGN KEY (`b`) REFERENCES `q` (`id`)"},
		},
		Triggers: []originalTrigger{
			{
				Name:      "tr",
				SQLMode:   "STRICT_TRANS_TABLES",
				Collation: "utf8mb4_general_ci",
				Create:    "CREATE TRIGGER `tr` AFTER INSERT ON `t_smgla_` FOR EACH ROW begin end",
			},
		},
	}
	steps := p.steps("t", "t_smgla_", "ONLY_FULL_GROUP_BY", "utf8mb4_bin")
	want := []restoreStep{
		{Purpose: "adding foreign keys", SQL: "alter table `t_smgla_` add constraint `fk1` FOREIGN KEY (`a`) REFERENCES `p` (`id`), add constraint `fk2` FOREIGN KEY (`b`) REFERENCES `q` (`id`)"},
		{Purpose: "setting original trigger tr sql_mode and collation", SQL: "set session sql_mode='STRICT_TRANS_TABLES', collation_connection='utf8mb4_general_ci'"},
		{Purpose: "adding original trigger tr", SQL: "CREATE TRIGGER `tr` AFTER INSERT ON `t_smgla_` FOR EACH ROW begin end", Trigger: true},
		{Purpose: "restoring session sql_mode and collation", SQL: "set session sql_mode='ONLY_FULL_GROUP_BY', collation_connection='utf8mb4_bin'"},
		{Purpose: "renaming the temp table", SQL: "alter table `t_smgla_` rename `t`"},
	}
	if len(steps) != len(want) {
		t.Fatalf("len = %d, want %d: %+v", len(steps), len(want), steps)
	}
	for i := range want {
		if steps[i] != want[i] {
			t.Errorf("step %d =\n%+v\nwant\n%+v", i, steps[i], want[i])
		}
	}
}

func TestRestorePlanStepsQuotes(t *testing.T) {
	p := restorePlan{
		Triggers: []originalTrigger{
			{Name: "tr", SQLMode: `a'b\c`, Collation: "x", Create: "CREATE TRIGGER `tr` AFTER INSERT ON `t` FOR EACH ROW begin end"},
		},
	}
	steps := p.steps("t", "t_smgla_", `d'e`, "y")
	if steps[0].SQL != `set session sql_mode='a\'b\\c', collation_connection='x'` {
		t.Errorf("trigger set: %s", steps[0].SQL)
	}
	if steps[2].SQL != `set session sql_mode='d\'e', collation_connection='y'` {
		t.Errorf("session set: %s", steps[2].SQL)
	}
}

func TestRecoverySQL(t *testing.T) {
	got := recoverySQL("db", "utf8mb4", []restoreStep{
		{Purpose: "fk", SQL: "alter table `t_smgla_` add constraint `fk` FOREIGN KEY (`a`) REFERENCES `p` (`id`)"},
		{Purpose: "set", SQL: "set session sql_mode='x', collation_connection='y'"},
		{Purpose: "trig", SQL: "CREATE TRIGGER `tr` AFTER INSERT ON `t_smgla_` FOR EACH ROW begin end", Trigger: true},
		{Purpose: "restore", SQL: "set session sql_mode='z', collation_connection='w'"},
		{Purpose: "rename", SQL: "alter table `t_smgla_` rename `t`"},
	})
	want := "use `db`;\n" +
		"set names `utf8mb4`;\n" +
		"set foreign_key_checks=0;\n" +
		"alter table `t_smgla_` add constraint `fk` FOREIGN KEY (`a`) REFERENCES `p` (`id`);\n" +
		"set session sql_mode='x', collation_connection='y';\n" +
		"DELIMITER $$\n" +
		"CREATE TRIGGER `tr` AFTER INSERT ON `t_smgla_` FOR EACH ROW begin end\n" +
		"$$\n" +
		"DELIMITER ;\n" +
		"set session sql_mode='z', collation_connection='w';\n" +
		"alter table `t_smgla_` rename `t`;\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestRestorePlanEqual(t *testing.T) {
	base := restorePlan{
		ForeignKeys: []foreignKey{{Name: "fk", Definition: "FOREIGN KEY (`a`) REFERENCES `p` (`id`)"}},
		Triggers:    []originalTrigger{{Name: "tr", SQLMode: "m", Collation: "c", Create: "sql"}},
		Inbound:     []inboundKey{{Schema: "db", Table: "child", Name: "fk_c", Columns: []string{"id"}}},
	}
	if !base.equal(base) {
		t.Fatal("equal to itself")
	}
	other := base
	other.ForeignKeys = []foreignKey{{Name: "fk2", Definition: base.ForeignKeys[0].Definition}}
	if base.equal(other) {
		t.Fatal("different foreign key name")
	}
	other = base
	other.Triggers = []originalTrigger{{Name: "tr", SQLMode: "m", Collation: "c", Create: "other"}}
	if base.equal(other) {
		t.Fatal("different trigger create")
	}
	other = base
	other.Inbound = []inboundKey{{Schema: "db", Table: "child", Name: "fk_c", Columns: []string{"id", "x"}}}
	if base.equal(other) {
		t.Fatal("different inbound columns")
	}
}

func TestCheckInboundKeys(t *testing.T) {
	key := inboundKey{Schema: "db", Table: "child", Name: "fk_child", Columns: []string{"id"}}
	mapping := pairs("id", "id")
	src := []column{{ColumnName: "id", ColumnType: "int"}}
	dst := []column{{ColumnName: "id", ColumnType: "int"}}
	indexes := []tableIndex{uindex("PRIMARY", colPart("id", 0))}

	if err := checkInboundKeys([]inboundKey{key}, mapping, src, dst, indexes); err != nil {
		t.Fatalf("passing: %v", err)
	}

	err := checkInboundKeys([]inboundKey{key}, pairs(), src, []column{}, nil)
	if err == nil || !strings.Contains(err.Error(), `referenced column "id" is not on the destination`) {
		t.Fatalf("missing column: %v", err)
	}

	err = checkInboundKeys([]inboundKey{key}, mapping, src, []column{{ColumnName: "id", ColumnType: "bigint"}}, indexes)
	if err == nil || !strings.Contains(err.Error(), `changes from int to bigint`) {
		t.Fatalf("type change: %v", err)
	}

	err = checkInboundKeys([]inboundKey{key}, mapping,
		[]column{{ColumnName: "id", ColumnType: "varchar(10)", CollationName: "utf8mb4_general_ci"}},
		[]column{{ColumnName: "id", ColumnType: "varchar(10)", CollationName: "utf8mb4_bin"}},
		indexes)
	if err == nil || !strings.Contains(err.Error(), "collation") {
		t.Fatalf("collation change: %v", err)
	}

	err = checkInboundKeys([]inboundKey{key}, mapping, src, dst, nil)
	if err == nil || !strings.Contains(err.Error(), "no index") {
		t.Fatalf("index dropped: %v", err)
	}

	comp := inboundKey{Schema: "db", Table: "child", Name: "fk_child", Columns: []string{"a", "b"}}
	compMap := pairs("a", "a", "b", "b")
	compSrc := []column{{ColumnName: "a", ColumnType: "int"}, {ColumnName: "b", ColumnType: "int"}}
	compDst := []column{{ColumnName: "a", ColumnType: "int"}, {ColumnName: "b", ColumnType: "int"}}
	err = checkInboundKeys([]inboundKey{comp}, compMap, compSrc, compDst, []tableIndex{uindex("k", colPart("b", 0), colPart("a", 0))})
	if err == nil || !strings.Contains(err.Error(), "no index") {
		t.Fatalf("different column order: %v", err)
	}

	err = checkInboundKeys([]inboundKey{key}, mapping, src, dst, []tableIndex{uindex("k", colPart("id", 4))})
	if err == nil || !strings.Contains(err.Error(), "no index") {
		t.Fatalf("prefix index: %v", err)
	}
}
