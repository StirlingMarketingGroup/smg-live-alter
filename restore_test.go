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
	}, true)
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

func twoTriggerPlan() restorePlan {
	return restorePlan{
		Triggers: []originalTrigger{
			{
				Name:      "tr1",
				SQLMode:   "MODE1",
				Collation: "col1",
				Original:  "CREATE TRIGGER `tr1` AFTER INSERT ON `t` FOR EACH ROW begin end",
				Create:    "CREATE TRIGGER `tr1` AFTER INSERT ON `t_smgla_` FOR EACH ROW begin end",
			},
			{
				Name:      "tr2",
				SQLMode:   "MODE2",
				Collation: "col2",
				Original:  "CREATE TRIGGER `tr2` AFTER UPDATE ON `t` FOR EACH ROW begin end",
				Create:    "CREATE TRIGGER `tr2` AFTER UPDATE ON `t_smgla_` FOR EACH ROW begin end",
			},
		},
	}
}

func TestAtomicSteps(t *testing.T) {
	steps := twoTriggerPlan().atomicSteps("t", "t_smgla_", "t_smgla_old_", "t_smgla_run_", "SESS", "bin")
	want := []string{
		"drop trigger `tr1`",
		"drop trigger `tr2`",
		"set session sql_mode='MODE1', collation_connection='col1'",
		"CREATE TRIGGER `tr1` AFTER INSERT ON `t_smgla_` FOR EACH ROW begin end",
		"set session sql_mode='MODE2', collation_connection='col2'",
		"CREATE TRIGGER `tr2` AFTER UPDATE ON `t_smgla_` FOR EACH ROW begin end",
		"set session sql_mode='SESS', collation_connection='bin'",
		"rename table `t` to `t_smgla_old_`, `t_smgla_` to `t`",
		"drop table `t_smgla_run_`",
	}
	if len(steps) != len(want) {
		t.Fatalf("len = %d, want %d: %+v", len(steps), len(want), steps)
	}
	for i := range want {
		if steps[i].SQL != want[i] {
			t.Errorf("step %d = %s, want %s", i, steps[i].SQL, want[i])
		}
	}
	if !steps[3].Trigger || !steps[5].Trigger {
		t.Errorf("creates should render as triggers: %+v", steps)
	}
	if !steps[7].renames || !steps[0].dropsOriginal || !steps[1].dropsOriginal || !steps[3].addsOriginal || !steps[5].addsOriginal {
		t.Errorf("progress flags: %+v", steps)
	}
}

func TestAtomicUndo(t *testing.T) {
	p := twoTriggerPlan()
	// Reachable points: every original is dropped before any is created.
	points := []struct{ dropped, created int }{{0, 0}, {1, 0}, {2, 0}, {2, 1}, {2, 2}}
	for _, pt := range points {
		steps := p.undoSteps(pt.dropped, pt.created, "SESS", "bin")
		var drops, creates []string
		for _, s := range steps {
			switch {
			case strings.HasPrefix(s.SQL, "drop trigger if exists "):
				drops = append(drops, s.SQL)
			case strings.HasPrefix(s.SQL, "CREATE TRIGGER "):
				creates = append(creates, s.SQL)
				if !s.Trigger {
					t.Errorf("(%d,%d) create not marked as a trigger: %s", pt.dropped, pt.created, s.SQL)
				}
			}
		}
		if len(drops) != pt.created {
			t.Errorf("(%d,%d) drops = %v", pt.dropped, pt.created, drops)
		}
		for i, sql := range drops {
			want := "drop trigger if exists " + quoteIdent(p.Triggers[i].Name)
			if sql != want {
				t.Errorf("(%d,%d) drop %d = %s, want %s", pt.dropped, pt.created, i, sql, want)
			}
		}
		if len(creates) != pt.dropped {
			t.Errorf("(%d,%d) creates = %v", pt.dropped, pt.created, creates)
		}
		for i, sql := range creates {
			if sql != p.Triggers[i].Original {
				t.Errorf("(%d,%d) create %d = %s, want original %s", pt.dropped, pt.created, i, sql, p.Triggers[i].Original)
			}
		}
		if steps[len(steps)-1].SQL != "set session sql_mode='SESS', collation_connection='bin'" {
			t.Errorf("(%d,%d) session restore = %s", pt.dropped, pt.created, steps[len(steps)-1].SQL)
		}
	}

	steps := p.undoSteps(2, 1, "SESS", "bin")
	want := []string{
		"drop trigger if exists `tr1`",
		"set session sql_mode='MODE1', collation_connection='col1'",
		"CREATE TRIGGER `tr1` AFTER INSERT ON `t` FOR EACH ROW begin end",
		"set session sql_mode='MODE2', collation_connection='col2'",
		"CREATE TRIGGER `tr2` AFTER UPDATE ON `t` FOR EACH ROW begin end",
		"set session sql_mode='SESS', collation_connection='bin'",
	}
	if len(steps) != len(want) {
		t.Fatalf("len = %d, want %d: %+v", len(steps), len(want), steps)
	}
	for i := range want {
		if steps[i].SQL != want[i] {
			t.Errorf("step %d = %s, want %s", i, steps[i].SQL, want[i])
		}
	}
}

func TestAtomicRecoverySQL(t *testing.T) {
	got := recoverySQL("db", "utf8mb4", twoTriggerPlan().atomicRecoverySteps("SESS", "bin"), false)
	if strings.Contains(got, "foreign_key_checks") {
		t.Fatalf("atomic recovery disables foreign key checks:\n%s", got)
	}
	want := "use `db`;\n" +
		"set names `utf8mb4`;\n" +
		"drop trigger if exists `tr1`;\n" +
		"set session sql_mode='MODE1', collation_connection='col1';\n" +
		"DELIMITER $$\n" +
		"CREATE TRIGGER `tr1` AFTER INSERT ON `t` FOR EACH ROW begin end\n" +
		"$$\n" +
		"DELIMITER ;\n" +
		"drop trigger if exists `tr2`;\n" +
		"set session sql_mode='MODE2', collation_connection='col2';\n" +
		"DELIMITER $$\n" +
		"CREATE TRIGGER `tr2` AFTER UPDATE ON `t` FOR EACH ROW begin end\n" +
		"$$\n" +
		"DELIMITER ;\n" +
		"set session sql_mode='SESS', collation_connection='bin';\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestChooseCutOver(t *testing.T) {
	if strategy, reason := chooseCutOver(restorePlan{}, "8.0.13"); strategy != cutOverAtomic || reason != "" {
		t.Fatalf("8.0.13: %v %q", strategy, reason)
	}
	if strategy, _ := chooseCutOver(restorePlan{}, "8.0.42"); strategy != cutOverAtomic {
		t.Fatalf("8.0.42: %v", strategy)
	}
	if strategy, _ := chooseCutOver(restorePlan{}, "8.0.42-aurora"); strategy != cutOverAtomic {
		t.Fatalf("aurora: %v", strategy)
	}
	if strategy, _ := chooseCutOver(restorePlan{}, "8.4.0"); strategy != cutOverAtomic {
		t.Fatalf("8.4.0: %v", strategy)
	}
	own := restorePlan{ForeignKeys: []foreignKey{{Name: "fk"}}}
	if strategy, reason := chooseCutOver(own, "8.0.42"); strategy != cutOverDropFirst || reason != "the table has foreign keys" {
		t.Fatalf("own foreign key: %v %q", strategy, reason)
	}
	inbound := restorePlan{Inbound: []inboundKey{{Schema: "db", Table: "child", Name: "fk"}}}
	if strategy, reason := chooseCutOver(inbound, "8.0.42"); strategy != cutOverDropFirst || reason != "the table has foreign keys" {
		t.Fatalf("inbound foreign key: %v %q", strategy, reason)
	}
	if strategy, reason := chooseCutOver(restorePlan{}, "8.0.12"); strategy != cutOverDropFirst || reason != "the server is older than 8.0.13" {
		t.Fatalf("8.0.12: %v %q", strategy, reason)
	}
	if strategy, reason := chooseCutOver(restorePlan{}, "not-a-version"); strategy != cutOverDropFirst || reason != "the server is older than 8.0.13" {
		t.Fatalf("unparsable: %v %q", strategy, reason)
	}
	if strategy, reason := chooseCutOver(own, "5.7.44"); strategy != cutOverDropFirst || reason != "the table has foreign keys" {
		t.Fatalf("foreign keys win over an old server: %v %q", strategy, reason)
	}
}

func TestParseMySQLVersion(t *testing.T) {
	major, minor, patch, ok := parseMySQLVersion("8.0.42-aurora")
	if !ok || major != 8 || minor != 0 || patch != 42 {
		t.Fatalf("aurora string: %d.%d.%d ok=%v", major, minor, patch, ok)
	}
	major, minor, patch, ok = parseMySQLVersion("8.0.42")
	if !ok || major != 8 || minor != 0 || patch != 42 {
		t.Fatalf("plain: %d.%d.%d ok=%v", major, minor, patch, ok)
	}
	if _, _, _, ok := parseMySQLVersion("8.0"); ok {
		t.Fatal("two components parsed")
	}
	if _, _, _, ok := parseMySQLVersion(""); ok {
		t.Fatal("empty parsed")
	}
}
