package main

import "testing"

func TestRetargetTrigger(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, create, triggerName, table, want string
	}{
		{
			name:        "body on the header line names other tables",
			create:      "CREATE DEFINER=`root`@`%` TRIGGER `t_counts` AFTER INSERT ON `t` FOR EACH ROW update `t_count` set `n`=`n`+1",
			triggerName: "t_counts",
			table:       "_t_new",
			want:        "CREATE DEFINER=`root`@`%` TRIGGER `t_counts` AFTER INSERT ON `_t_new` FOR EACH ROW update `t_count` set `n`=`n`+1",
		},
		{
			name:        "body on its own lines",
			create:      "CREATE DEFINER=`root`@`%` TRIGGER `t_bi` BEFORE UPDATE ON `t` FOR EACH ROW begin\nset new.`b` = 1;\nend",
			triggerName: "t_bi",
			table:       "_t_new",
			want:        "CREATE DEFINER=`root`@`%` TRIGGER `t_bi` BEFORE UPDATE ON `_t_new` FOR EACH ROW begin\nset new.`b` = 1;\nend",
		},
		{
			name:        "trigger order clause and a body that mentions ON",
			create:      "CREATE DEFINER=`a`@`b` TRIGGER `x` AFTER DELETE ON `my``t` FOR EACH ROW FOLLOWS `y` insert into `log` select 1 from `z` join `w` ON `z`.`a` = `w`.`a`",
			triggerName: "x",
			table:       "_t_new",
			want:        "CREATE DEFINER=`a`@`b` TRIGGER `x` AFTER DELETE ON `_t_new` FOR EACH ROW FOLLOWS `y` insert into `log` select 1 from `z` join `w` ON `z`.`a` = `w`.`a`",
		},
		{
			name:        "name rewrite",
			create:      "CREATE DEFINER=`root`@`%` TRIGGER `t_counts` AFTER INSERT ON `t` FOR EACH ROW update `t_count` set `n`=`n`+1",
			triggerName: "smgla_probe_0",
			table:       "p",
			want:        "CREATE DEFINER=`root`@`%` TRIGGER `smgla_probe_0` AFTER INSERT ON `p` FOR EACH ROW update `t_count` set `n`=`n`+1",
		},
		{
			name:        "backtick-escaped name",
			create:      "CREATE DEFINER=`root`@`%` TRIGGER `my``trig` AFTER INSERT ON `my``t` FOR EACH ROW set new.`a`=1",
			triggerName: "other`name",
			table:       "_t_new",
			want:        "CREATE DEFINER=`root`@`%` TRIGGER `other``name` AFTER INSERT ON `_t_new` FOR EACH ROW set new.`a`=1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := retargetTrigger(tt.create, tt.triggerName, tt.table)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}

	if _, err := retargetTrigger("not a trigger", "x", "_t_new"); err == nil {
		t.Error("want an error for a statement with no ON ... FOR EACH ROW")
	}
}
