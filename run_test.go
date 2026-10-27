package main

import (
	"strings"
	"testing"
)

func TestNormalizeCreate(t *testing.T) {
	in := "CREATE TABLE `t` (\n  `id` int NOT NULL AUTO_INCREMENT\n) ENGINE=InnoDB AUTO_INCREMENT=42 DEFAULT CHARSET=utf8mb4"
	want := "CREATE TABLE `t` (\n  `id` int NOT NULL AUTO_INCREMENT\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	if got := normalizeCreate(in); got != want {
		t.Fatalf("got %s", got)
	}
	if got := normalizeCreate(want); got != want {
		t.Fatalf("normalized again: %s", got)
	}
	plain := "CREATE TABLE `t` (`id` int) ENGINE=InnoDB"
	if got := normalizeCreate(plain); got != plain {
		t.Fatalf("changed a statement with no table option: %s", got)
	}
}

func TestRunTableDDL(t *testing.T) {
	got := runTableDDL("emails_smgla_run_", []column{{ColumnType: "binary(16)"}})
	want := "create table `emails_smgla_run_` (`alter_text` longtext not null,`source_create` longtext not null,`temp_create` longtext not null,`k0` binary(16),`copy_done` tinyint not null default 0,`readers` text not null default ('')) engine=InnoDB"
	if got != want {
		t.Fatalf("binary key:\n got %s\nwant %s", got, want)
	}

	got = runTableDDL("t_smgla_run_", []column{
		{ColumnType: "bigint unsigned"},
		{ColumnType: "varchar(32)", CharacterSetName: "utf8mb4", CollationName: "utf8mb4_unicode_ci"},
	})
	want = "create table `t_smgla_run_` (`alter_text` longtext not null,`source_create` longtext not null,`temp_create` longtext not null,`k0` bigint unsigned,`k1` varchar(32) character set utf8mb4 collate utf8mb4_unicode_ci,`copy_done` tinyint not null default 0,`readers` text not null default ('')) engine=InnoDB"
	if got != want {
		t.Fatalf("two-column key:\n got %s\nwant %s", got, want)
	}
}

func TestDecideResume(t *testing.T) {
	base := func() resumeFacts {
		f := resumeFacts{
			names: resumeNames{
				table:         "emails",
				temp:          "emails_smgla_",
				run:           "emails_smgla_run_",
				deleteTrigger: "emails_after_delete_smgla_",
				updateTrigger: "emails_after_update_smgla_",
				insertTrigger: "emails_after_insert_smgla_",
			},
			runExists:          true,
			runEngine:          "InnoDB",
			hasRow:             true,
			alterText:          "alter table `emails` add `x` int",
			storedAlter:        "alter table `emails` add `x` int",
			storedSourceCreate: "CREATE TABLE `emails` (\n  `id` binary(16) NOT NULL\n) ENGINE=InnoDB",
			sourceCreateNow:    "CREATE TABLE `emails` (\n  `id` binary(16) NOT NULL\n) ENGINE=InnoDB AUTO_INCREMENT=9",
			tempExists:         true,
			storedTempCreate:   "CREATE TABLE `emails_smgla_` (\n  `id` binary(16) NOT NULL\n) ENGINE=InnoDB",
			tempCreateNow:      "CREATE TABLE `emails_smgla_` (\n  `id` binary(16) NOT NULL\n) ENGINE=InnoDB AUTO_INCREMENT=3",
			keyColumns:         []runColumn{{Name: "k0", ColumnType: "binary(16)"}},
			traversalKey:       []column{{ColumnName: "id", ColumnType: "binary(16)"}},
			sqlMode:            "NO_ENGINE_SUBSTITUTION",
			readers:            "reader-a,reader-b",
			cursorNULL:         true,
			wantTriggers: []wantTrigger{
				{Name: "emails_after_delete_smgla_", Event: "DELETE", Body: "delete from t"},
				{Name: "emails_after_update_smgla_", Event: "UPDATE", Body: "update t"},
				{Name: "emails_after_insert_smgla_", Event: "INSERT", Body: "insert into t"},
			},
		}
		f.triggers = []triggerRow{
			{Name: f.names.deleteTrigger, Table: f.names.table, Timing: "AFTER", Event: "DELETE", Statement: "delete from t", SQLMode: f.sqlMode},
			{Name: f.names.updateTrigger, Table: f.names.table, Timing: "AFTER", Event: "UPDATE", Statement: "update t", SQLMode: f.sqlMode},
			{Name: f.names.insertTrigger, Table: f.names.table, Timing: "AFTER", Event: "INSERT", Statement: "insert into t", SQLMode: f.sqlMode},
		}
		return f
	}

	t.Run("no cursor yet", func(t *testing.T) {
		state, err := decideResume(base())
		if err != nil {
			t.Fatal(err)
		}
		if state.copyDone || state.hasCursor {
			t.Fatalf("state = %+v", state)
		}
		if len(state.readers) != 2 || state.readers[0] != "reader-a" || state.readers[1] != "reader-b" {
			t.Fatalf("readers = %#v", state.readers)
		}
	})

	t.Run("cursor", func(t *testing.T) {
		f := base()
		f.cursorNULL = false
		state, err := decideResume(f)
		if err != nil {
			t.Fatal(err)
		}
		if state.copyDone || !state.hasCursor {
			t.Fatalf("state = %+v", state)
		}
	})

	t.Run("copy done", func(t *testing.T) {
		f := base()
		f.copyDone = true
		state, err := decideResume(f)
		if err != nil {
			t.Fatal(err)
		}
		if !state.copyDone {
			t.Fatalf("state = %+v", state)
		}
	})

	refusals := []struct {
		name   string
		change func(*resumeFacts)
		want   string
	}{
		{
			name: "missing run row",
			change: func(f *resumeFacts) {
				f.hasRow = false
			},
			want: "cannot resume emails: no run record for emails_smgla_; rerun with -fresh to start over",
		},
		{
			name: "run table missing",
			change: func(f *resumeFacts) {
				f.runExists = false
				f.hasRow = false
			},
			want: "cannot resume emails: no run record for emails_smgla_; rerun with -fresh to start over",
		},
		{
			name: "run table not innodb",
			change: func(f *resumeFacts) {
				f.runEngine = "MyISAM"
			},
			want: "cannot resume emails: run table emails_smgla_run_ uses MyISAM, not InnoDB; rerun with -fresh to start over",
		},
		{
			name: "different alter text",
			change: func(f *resumeFacts) {
				f.alterText = "alter table `emails` drop `x`"
			},
			want: "cannot resume emails: the alter text differs from the saved run; rerun with -fresh to start over",
		},
		{
			name: "changed source create",
			change: func(f *resumeFacts) {
				f.sourceCreateNow = "CREATE TABLE `emails` (\n  `id` binary(16) NOT NULL,\n  `y` int\n) ENGINE=InnoDB"
			},
			want: "cannot resume emails: the source table changed since the run started; rerun with -fresh to start over",
		},
		{
			name: "changed temp create",
			change: func(f *resumeFacts) {
				f.tempCreateNow = "CREATE TABLE `emails_smgla_` (\n  `id` binary(16) NOT NULL,\n  `y` int\n) ENGINE=InnoDB"
			},
			want: "cannot resume emails: temp table emails_smgla_ changed since the run started; rerun with -fresh to start over",
		},
		{
			name: "cursor column type mismatch",
			change: func(f *resumeFacts) {
				f.keyColumns[0].ColumnType = "varchar(32)"
				f.keyColumns[0].Charset = "utf8mb4"
				f.keyColumns[0].Collation = "utf8mb4_general_ci"
			},
			want: "cannot resume emails: cursor column k0 (varchar(32), character set \"utf8mb4\", collation \"utf8mb4_general_ci\") does not match primary key column id (binary(16), character set \"\", collation \"\"); rerun with -fresh to start over",
		},
		{
			name: "wrong column count",
			change: func(f *resumeFacts) {
				f.traversalKey = append(f.traversalKey, column{ColumnName: "seq", ColumnType: "bigint"})
			},
			want: "cannot resume emails: saved cursor columns are 1, primary key columns are 2; rerun with -fresh to start over",
		},
		{
			name: "missing trigger",
			change: func(f *resumeFacts) {
				f.triggers = f.triggers[1:]
			},
			want: "cannot resume emails: mirror trigger emails_after_delete_smgla_ is missing; rerun with -fresh to start over",
		},
		{
			name: "trigger body differs",
			change: func(f *resumeFacts) {
				f.triggers[0].Statement = "delete from other"
			},
			want: "cannot resume emails: mirror trigger emails_after_delete_smgla_ body differs; rerun with -fresh to start over",
		},
		{
			name: "trigger sql_mode differs",
			change: func(f *resumeFacts) {
				f.triggers[0].SQLMode = "ANSI_QUOTES"
			},
			want: "cannot resume emails: mirror trigger emails_after_delete_smgla_ sql_mode is \"ANSI_QUOTES\", this session is \"NO_ENGINE_SUBSTITUTION\"; rerun with -fresh to start over",
		},
	}

	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			f := base()
			tt.change(&f)
			_, err := decideResume(f)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if err.Error() != tt.want {
				t.Fatalf("got %s", err.Error())
			}
			if !strings.HasSuffix(err.Error(), "; rerun with -fresh to start over") {
				t.Fatalf("suffix: %s", err.Error())
			}
		})
	}
}
