# SMG Live Alter

### Background

Yet another tool to apply alters to MySQL tables without down time. Why another one? Surely Percona, Facebook, and GitHub have all covered this well enough their versions, but for our purposes, just not quite.

Not to hate on Percona or their pt-online-schema-change, but that tool has just enough small problems with it to be more than annoying for our use. Particularly how it treats triggers on tables. We rely on MySQL triggers on our tables to keep tons of denormalized statistics and other information consistent. Now, the Percona tool does claim to support keeping your triggers where they are, but I have yet to see this actually work in real world use except for *super* specific cases, like the table has to be under a certain size, not have foreign keys referencing it, and can't be a day ending in 'y', which in our case is most of our tables.

The tool preserves existing triggers during the schema change.

Another problem with that tool and almost all of the others is that they're quite complicated, which I why I think bugs in the Percona tool exist (like Binary columns being treated as the table's charset and throwing errors about values not being valid UTF8), but not this one.

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

  - `-c` your connections file (default `~/.config/smgla/connections.yaml` on Linux, `~/Library/Application Support/smgla/connections.yaml` on macOS, more info below)
  - `-suffix` suffix of the temp table used for initial creation before the swap and drop (default `_smgla_`)
  - `-r` value
        starting rows per copy chunk; adjusted to keep each chunk near 500ms (default 1000)

As you can see, there's not a lot of options here. Yay simplicity!

There’re two arguments I can see here that may need a tad explained, however:

1. `-suffix` - This is simply the suffix that this tool uses on the temp tables it generates. This should be something that won't collide with other table names. Example: if you have two tables, one named `orders` (that's the one being altered) and another table named `ordersplace`, then don't set your suffix to "placed" because it will drop `ordersplace` thinking it's a left-over temp table from a previous run.

The tool parses the table name and other things from the alter query, so there's no need to give that as a separate option (looking at you two, GitHub and Percona).

---

### How it works

Unlike the ultra-fancy way that the GitHub tool works, this one works more closely to how the rest work; with triggers. Now the fact that this uses triggers means that it will **only work with MySQL version 5.7.2 and above**. Another requirement of this tool is that it **only works with tables that have primary keys**. The copy walks that key in index order, and every key column has to be an integer, a decimal, or a char/binary string. The cursor lives in user variables, and MySQL will not keep a date, an enum, or a float in one of those and still order it like the index.

The tool refuses an alter it cannot mirror. A foreign key on the table whose update or delete rule is `CASCADE`, `SET NULL`, or `SET DEFAULT` changes rows without firing triggers. A foreign key that references the table itself is refused, because the probe would resolve it against the old schema. Unique coverage also counts a derived column: a generated column whose expression is a conditional projection (`` `col` ``, or `if(<cond>, `col`, NULL)` / `if(<cond>, NULL, `col`)`) of a copied column, with the generated type equal to the projected column's type on each side, and a condition that is one integer column compared with an integer literal, unchanged by the alter. Any other generated column in a destination unique index has to be covered without that proof. Every destination unique index, the primary key included, has to be covered by a source unique index. A source index covers it when every source part maps to one of the destination index's parts, so extra destination parts need no proof. A functional source index covers nothing, because the same expression can yield different values after a type or charset change. A collation change on a unique column is refused, because distinct values can become equal. A unique-column conversion that does not keep values (for example `varbinary(2)` to `binary(2)`) is refused for every unique index, the primary key included, because two distinct source rows could land on one destination key. A generated source column that is stored on the destination is refused, because triggers cannot read generated columns. `destinationKey` requires a destination primary key copied from the source. The storage engine cannot change. Other tables' foreign keys that point at this table re-attach by name after the rename, so every referenced column has to map to a destination column with the same name, type, character set, and collation, and the destination has to keep an index whose leading columns are those columns in that order with no prefix length.

Before any mirror trigger exists, the restore plan is compiled against a probe table (`<table><suffix>probe_`) created `LIKE` the altered temp table. That probe proves the foreign key definitions and the original trigger bodies compile against the altered columns. It does not prove the rename, permissions, or anything another session changes. The tool assumes no other DDL runs on the table or its foreign key neighbors during a run.

Each chunk is copied inside the server: `insert into` the temp table `select ... from` the original table `where` a primary-key range `lock in share mode`, with a no-op `on duplicate key update` so a row the triggers wrote first stays. That duplicate is the same source row, because of the unique-index check above, and the no-op keeps it without a warning, so any warning the copy raises is a real problem. The cursor and the chunk's end key sit in session variables (`@smgla_k0..` and `@smgla_e0..`), one per primary key column, so the key values never come back to the client and they compare with the index's own types and collations. The range is written out lexicographically, `(a > @k0) or (a = @k0 and b > @k1)`, which MySQL plans as a range on the primary key. The first chunk has no lower bound and the last chunk has no upper bound. `lock in share mode` reads that range at its latest committed state and holds it until the statement finishes, so a delete can't land between the read and the insert. The chunk starts at `-r` rows and grows or shrinks to keep each copy statement near 500ms, and it doesn't grow past 10000 rows. A chunk that raises any warning stops the tool. That rule is stricter than the triggers, because the copy governs rows that already exist.

Triggers on the original table mirror inserts, updates, and deletes into that one temp table. They are created on one session, the delete trigger first, then the update trigger, then the insert trigger, under the server's global `sql_mode`. A trigger keeps the `sql_mode` it was created with, and the application's sessions use that mode, so the triggers store what the application's writes store after the swap. None of them uses `IGNORE`: a write the destination rejects fails the application's statement. The delete trigger deletes the temp row whose key equals the old source key. The update trigger updates that temp row in place, which moves the row when its primary key changes and leaves an added column's default as it is, then inserts the new row only when no temp row has the new key. The insert trigger inserts the new row. The copy and the swap run on that same session, so `foreign_key_checks`, `sql_mode`, the copy's isolation level, and the cursor variables stay put.

Once the data is all copied into the temp table, the restore plan is read again. If the source's triggers or foreign keys changed during the copy, the tool stops before the drop; rerun it. That re-check is a cheap guard for the no-other-DDL assumption, not a proof. The same post-drop steps are written to `smgla-recovery-<table>-<unix seconds>.sql` in the working directory. If a step fails after the drop, finish from that step onward by pasting the remaining statements from that file into the mysql client.

1. The old table is dropped. This happens now to avoid consistency problems. We understand that dropping this table first and then doing other things before the temp table is renamed will cause a very small amount of time that no table exists with the original table's name, but we took this tradeoff to ensure the data is as consistent as possible. Essentially, we require that the application using the table in production retries its queries if the table does not exist

2. Foreign keys go back onto the temp table, one `ALTER TABLE` from the restore plan. Check constraints are already there: a generated name `<table>_chk_<n>` is created on the temp table as `<temp>_chk_<n>`, and MySQL renames it back with the table. A `DROP CHECK`, `ALTER CHECK`, `DROP CONSTRAINT`, or `ALTER CONSTRAINT` in the alter is rewritten to that temp name before the alter runs. A custom check name can't be kept, so it gets a generated one. Foreign keys are left off the temp table at creation because their names are unique to the whole database, and they're added here, after the original table is dropped, with foreign key checks turned off.

3. Restore the original triggers onto the temp table, in `ACTION_ORDER`, each under the `sql_mode` and `collation_connection` it was created with. The table name in the `ON … FOR EACH ROW` header is rewritten to the temp table. The session's `sql_mode` and `collation_connection` are put back afterwards. We do this before the rename because triggers are *ultra-important* for us, and we can't have the table being written to without our triggers, so we'd rather it not exist yet.

4. Rename the temp table to the original table's name.

5. ???

6. Profit

And that's it! That's our SMG Live Alter table, hope you like it as much as I do.
