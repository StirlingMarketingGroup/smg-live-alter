# SMG Live Alter

Change a big, busy MySQL table while the app keeps writing to it. Your triggers keep firing, the foreign keys that point at the table survive, and you don't need the binary log.

### Proven on production

Every run below changed every ID column on a live Aurora MySQL 3 table (binary log off) while the app kept writing to it. Each ran from one EC2 box in the same region.

| Table | Rows | Size | Indexes | Triggers | Time |
| --- | ---: | ---: | ---: | ---: | ---: |
| `emails` | 17.4M | 44 GB | 30 | 0 | 1h 50m |
| `emailopens` | 26.4M | 11 GB | 11 | 0 | 46m 43s |
| `emailclicks` | 2.7M | 1.2 GB | 11 | 0 | 4m 27s |
| `orderlineitems` | 1.5M | 1.5 GB | 26 | 5 | 6m 18s |
| `cartitems` | 0.8M | 0.3 GB | 9 | 0 | 1m 00s |

During the 1h 50m `emails` copy, the Aurora reader's lag stayed at 4–25 ms and the copy never had to pause. At the swap, the table name was missing for about a second.

### Why it's fast

- **Rows never leave the server.** Each chunk is one `insert … select` on the writer, so no row crosses the network and nothing is parsed in Go.
- **Chunks size themselves.** Each chunk aims for about 500 ms and grows up to 10,000 rows, so a narrow table moves in big bites and a wide one in small ones.
- **It reads by the primary key.** Every chunk is a range on the primary key, so MySQL never scans the table to find where to start.

### What it handles

- **No binary log needed.** gh-ost reads the binary log, so it can't run on a server with the log turned off, like our Aurora cluster. This tool only needs triggers.
- **Your triggers keep working.** Before the new table takes the old name, the tool puts every existing trigger back on it, in its original order and with its original `sql_mode` and collation. No write ever lands on the table without them.
- **Foreign keys into the table survive.** Other tables' foreign keys follow the table through the swap, and the tool checks that they will still fit before it starts.
- **With no foreign keys, queries wait for the swap.** The tool locks the table, moves the triggers, and renames while it holds that lock, so other sessions wait and then run on the new table. The lock also takes every table the table's triggers use. The tool caps that wait at 10 seconds. The login needs the `LOCK TABLES` privilege.
- **With foreign keys, the name is missing between the drop and the rename.** A foreign key follows a renamed table, and its name cannot exist on two tables, so the old table is dropped first. Queries in that window fail and have to be retried.
- **It is gentle on Aurora readers.** The copy waits whenever a reader falls more than `-max-lag` behind.
- **It can stop and pick up where it left off.** Stop it at any point and rerun the same alter; it continues from the last chunk.
- **It refuses what it can't prove.** If a change could lose or merge rows, or a trigger or foreign key wouldn't survive it, the tool stops before it touches anything. The rules are in "How it works" below.

### Background

Yet another tool to apply alters to MySQL tables without downtime. Why another one? Percona, Facebook, and GitHub have all covered this well enough for their uses, but for ours, not quite.

Percona's pt-online-schema-change has just enough small problems to be more than annoying for us, particularly in how it treats triggers. We rely on MySQL triggers to keep tons of denormalized statistics and other information consistent. The Percona tool claims to keep your triggers where they are, but I have yet to see that work in real use except in *super* specific cases: the table has to be under a certain size, can't have foreign keys referencing it, and can't be a day ending in 'y'. That's most of our tables.

The other tools are also quite complicated, which I think is why bugs like the Percona tool's (binary columns treated as the table's charset, then rejected as invalid UTF-8) exist there and not here.

---
### Prerequisites

Building from source requires Go 1.26.0 or newer.

Go's installing instructions can be found here https://golang.org/doc/install#install

Once Go is installed, and you've added the go/bin folder to your path, you can install `smg-live-alter`.

```
go version # verify >=1.26.0
go install github.com/StirlingMarketingGroup/smg-live-alter/v2@latest
smg-live-alter -help
```

---

## Usage

```shell
smg-live-alter [flags] 'user:pass@(host)/dbname'
# or, with a connections file
smg-live-alter [flags] localhost
```
### Flags:

  - `-c` your connections file (default `~/.config/smgla/connections.yaml` on Linux, `~/Library/Application Support/smgla/connections.yaml` on macOS; see "Connections file" below)
  - `-suffix` suffix of the temp table used for initial creation before the swap and drop (default `_smgla_`)
  - `-r` value
        starting rows per copy chunk; adjusted to keep each chunk near 500ms (default 1000)
  - `-max-lag` pause the copy while an Aurora reader lags more than this; 0 turns pacing off (default 1s)
  - `-fresh` discard a stopped run of this alter and copy from the first row
  - `-forget-readers` clear the Aurora readers a resumed run is waiting on

As you can see, there's not a lot of options here. Yay simplicity!

There’re two arguments I can see here that may need a tad explained, however:

1. `-suffix` - This is simply the suffix that this tool uses on the temp tables it generates. This should be something that won't collide with other table names. Example: if you have two tables, one named `orders` (that's the one being altered) and another table named `ordersplace`, then don't set your suffix to "placed" because it will drop `ordersplace` thinking it's a left-over temp table from a previous run.

The tool parses the table name and other things from the alter query, so there's no need to give that as a separate option (looking at you two, GitHub and Percona).

### Connections file

Name a connection once and pass its name instead of a DSN:

```yaml
prod:
  user: "web"
  pass: "…"
  host: "db.example.com:3306"
  schema: sterling
  params:
    lock_wait_timeout: "10"
```

Each entry under `params` goes into the DSN, and the MySQL driver sets it as a session variable. `lock_wait_timeout: "10"` makes the drop and rename give up after 10 seconds instead of queuing every other query behind a long lock wait.

The login needs the `TRIGGER` privilege on the table, and `LOCK TABLES` when the table has no foreign keys. If the table's own triggers are owned by another account, the tool re-creates them under that owner, which needs `SET_USER_ID` or `SUPER`. On Aurora, the admin login has it through `rds_superuser_role`.

---

### How it works

Unlike the ultra-fancy way that the GitHub tool works, this one works more closely to how the rest work; with triggers. Now the fact that this uses triggers means that it will **only work with MySQL version 5.7.2 and above**. Another requirement of this tool is that it **only works with tables that have primary keys**. The copy walks that key in index order, and every key column has to be an integer, a decimal, or a char/binary string. The cursor lives in user variables, and MySQL will not keep a date, an enum, or a float in one of those and still order it like the index.

The tool refuses an alter it cannot mirror. A foreign key on the table whose update or delete rule is `CASCADE`, `SET NULL`, or `SET DEFAULT` changes rows without firing triggers. A foreign key that references the table itself is refused, because the probe would resolve it against the old schema. The temp table starts without the table's own foreign keys, and the originals are added back after the swap, so an alter can't drop one. Drop it first with a plain `ALTER TABLE … DROP FOREIGN KEY`, which only changes metadata. Unique coverage also counts a derived column: a generated column whose expression is a conditional projection (`` `col` ``, or `if(<cond>, `col`, NULL)` / `if(<cond>, NULL, `col`)`) of a copied column, with the generated type equal to the projected column's type on each side, and a condition that is one integer column compared with an integer literal, unchanged by the alter. Any other generated column in a destination unique index has to be covered without that proof. Every destination unique index, the primary key included, has to be covered by a source unique index. A source index covers it when every source part maps to one of the destination index's parts, so extra destination parts need no proof. A functional source index covers nothing, because the same expression can yield different values after a type or charset change. A collation change on a unique column is refused, because distinct values can become equal. A unique-column conversion that does not keep values (for example `varbinary(2)` to `binary(2)`) is refused for every unique index, the primary key included, because two distinct source rows could land on one destination key. A generated source column that is stored on the destination is refused, because triggers cannot read generated columns. `destinationKey` requires a destination primary key copied from the source. The storage engine cannot change. Other tables' foreign keys that point at this table re-attach by name after the rename, so every referenced column has to map to a destination column with the same name, type, character set, and collation, and the destination has to keep an index whose leading columns are those columns in that order with no prefix length.

Before any mirror trigger exists, the restore plan is compiled against a probe table (`<table><suffix>probe_`) created `LIKE` the altered temp table. That probe proves the foreign key definitions and the original trigger bodies compile against the altered columns. It does not prove the rename, permissions, or anything another session changes. The tool assumes no other DDL runs on the table or its foreign key neighbors during a run.

Each chunk is copied inside the server: `insert into` the temp table `select ... from` the original table `where` a primary-key range `lock in share mode`, with a no-op `on duplicate key update` so a row the triggers wrote first stays. That duplicate is the same source row, because of the unique-index check above, and the no-op keeps it without a warning, so any warning the copy raises is a real problem. The cursor and the chunk's end key sit in session variables (`@smgla_k0..` and `@smgla_e0..`), one per primary key column, so the key values never come back to the client and they compare with the index's own types and collations. The range is written out lexicographically, `(a > @k0) or (a = @k0 and b > @k1)`, which MySQL plans as a range on the primary key. The first chunk has no lower bound and the last chunk has no upper bound. `lock in share mode` reads that range at its latest committed state and holds it until the chunk's transaction commits, so a delete can't land between the read and the insert. The chunk starts at `-r` rows and grows or shrinks to keep each copy statement near 500ms, and it doesn't grow past 10000 rows. A chunk that raises any warning stops the tool. That rule is stricter than the triggers, because the copy governs rows that already exist.

Before each chunk the copy waits while any Aurora reader lags more than `-max-lag` (default 1s; `0` turns the wait off). No flag is needed to pace. The wait reads `information_schema.replica_host_status` on the writer. A reader is behind when its reported lag is over that limit, or when its status row is more than 10s old. A healthy reader's row is refreshed about once a second, so a younger row is not lag, and a row older than 10s is logged as the reader stopped reporting. A reader this run has already seen that then disappears pauses the copy too. The time spent waiting is not part of the chunk time, so a pause does not shrink the chunk. A server without that table is not Aurora, and the wait turns itself off. The first check cannot see a reader that was already gone, and `-forget-readers` clears the readers a resumed run is waiting on when one was removed on purpose. With no stopped run to resume, `-forget-readers` has no effect.

The source table has to be InnoDB. Each chunk is one transaction: the copy, the warning check, the checkpoint, then commit. The share locks are held until that commit. If a chunk raises a warning, the tool exits and InnoDB rolls the chunk back, so the next run does not keep it. A full chunk writes its end key into the run table `<table><suffix>run_`. The last partial chunk sets the run's `copy_done`. The run table also stores the alter text, the source and temp `SHOW CREATE TABLE` statements with the table `AUTO_INCREMENT` option removed (inserts move that counter), and the Aurora reader names seen during the run, including across resumes. Those two statements are read once, after the session `sql_mode` is set to the global `sql_mode`, and the row is inserted then, before any mirror trigger. The run table is dropped after the rename. It is not in the recovery file.

No flag is needed to resume. When the run table exists, has its row, and that row's alter text is this alter, the tool keeps the temp table and the mirror triggers and continues the copy when they are exactly this alter's. It logs that it found a stopped run. A run table whose alter text differs starts fresh, and the tool logs that. `-fresh` discards a stopped run of this alter and copies from the first row. The full check, in order, is that the run row exists, the run table is InnoDB, the alter text matches, the source and temp create statements still match, the checkpoint columns match the primary key, and the three mirror triggers are still the after delete, update, and insert triggers this run would create, with the same bodies and the same `sql_mode`. A mismatch stops the run; rerun with `-fresh` to start over. A temp table left by a run that has no run record cannot be resumed. When the copy already finished, the tool goes straight to the swap prompt. Otherwise it continues after the saved key, or from the first row when no chunk has been checkpointed. The progress bar still starts at 0, and its total is still the source row count. A fresh run drops any previous run table, triggers, and temp table before it starts. A resumed run drops only the probe table.

Answering `n` at `do the drop/swap?` leaves the temp table, the mirror triggers, and the run record in place. Run again to continue.

Triggers on the original table mirror inserts, updates, and deletes into that one temp table. They are created on one session, the delete trigger first, then the update trigger, then the insert trigger, under the server's global `sql_mode`. A trigger keeps the `sql_mode` it was created with, and the application's sessions use that mode, so the triggers store what the application's writes store after the swap. None of them uses `IGNORE`: a write the destination rejects fails the application's statement. The delete trigger deletes the temp row whose key equals the old source key. The update trigger updates that temp row in place, which moves the row when its primary key changes and leaves an added column's default as it is, then inserts the new row only when no temp row has the new key. The insert trigger inserts the new row. The copy and the swap run on that same session, so `foreign_key_checks`, `sql_mode`, the copy's isolation level, and the cursor variables stay put.

Once the data is all copied into the temp table, the restore plan is read again. If the source's triggers or foreign keys changed during the copy, the tool stops before the swap; rerun it. That re-check is a cheap guard for the no-other-DDL assumption, not a proof. The recovery statements are written to `smgla-recovery-<table>-<unix seconds>.sql` in the working directory before anything is dropped or locked.

When the table has no foreign keys, on MySQL 8.0.13 or newer, the swap holds `LOCK TABLES`. Other sessions wait, and they run on the new table when the lock is released. The login needs the `LOCK TABLES` privilege. The lock also takes every table the table's triggers use, and the tool caps the wait at 10 seconds.

1. Lock the original table, the temp table, and the run table. The run table is locked so it can be dropped before the unlock.
2. Drop each original trigger, then create it on the temp table under the `sql_mode` and `collation_connection` it was created with. The mirror triggers stay on the original table until the rename.
3. Rename the original table out of the way and the temp table into its name, in one `RENAME TABLE`, then drop the run table.
4. Unlock, put `lock_wait_timeout` back, and drop the previous table.

If a step fails before that rename, the tool puts the original triggers back and unlocks. The temp table, the mirror triggers, and the run record stay, so the next run resumes at the swap prompt. The recovery file is that undo. If the undo fails, run the file to put the triggers back on the original table, then rerun.

When the table has foreign keys, or the server is older than 8.0.13, the old table is dropped first. A foreign key follows a renamed table, and a foreign key name cannot sit on both tables, so one rename would point other tables at the old schema. The recovery file is the remaining forward steps. If a step fails after the drop, finish from that step onward by pasting the remaining statements from that file into the mysql client.

1. The old table is dropped. Until the rename below, no table has the original name, and queries fail. Retry them.
2. Foreign keys go back onto the temp table, one `ALTER TABLE` from the restore plan. Check constraints are already there: a generated name `<table>_chk_<n>` is created on the temp table as `<temp>_chk_<n>`, and MySQL renames it back with the table. A `DROP CHECK`, `ALTER CHECK`, `DROP CONSTRAINT`, or `ALTER CONSTRAINT` in the alter is rewritten to that temp name before the alter runs. A custom check name can't be kept, so it gets a generated one. Foreign keys are left off the temp table at creation because their names are unique to the whole database, and they're added here, after the original table is dropped, with foreign key checks turned off.
3. Restore the original triggers onto the temp table, in `ACTION_ORDER`, each under the `sql_mode` and `collation_connection` it was created with. The table name in the `ON … FOR EACH ROW` header is rewritten to the temp table. The session's `sql_mode` and `collation_connection` are put back afterwards. The triggers are back before the rename, so the table is not written without them.
4. Rename the temp table to the original table's name.

5. ???

6. Profit

And that's it! That's our SMG Live Alter table, hope you like it as much as I do.
