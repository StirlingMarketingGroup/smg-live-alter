# SMG Live Alter

### Background

Yet another tool to apply alters to MySQL tables without down time. Why another one? Surely Percona, Facebook, and GitHub have all covered this well enough their versions, but for our purposes, just not quite.

Not to hate on Percona or their pt-online-schema-change, but that tool has just enough small problems with it to be more than annoying for our use. Particularly how it treats triggers on tables. We rely on MySQL triggers on our tables to keep tons of denormalized statistics and other information consistent. Now, the Percona tool does claim to support keeping your triggers where they are, but I have yet to see this actually work in real world use except for *super* specific cases, like the table has to be under a certain size, not have foreign keys referencing it, and can't be a day ending in 'y', which in our case is most of our tables.

As a workaround for those issues, I've been copying the existing triggers for tables being altered into Workbench and then executing the apply statements as quickly as possible after the alter finishes, but with critical our triggers have become, that amount of potential downtime is not okay.

Another problem with that tool and almost all of the others is that they're quite complicated, which I why I think bugs in the Percona tool exist (like Binary columns being treated as the table's charset and throwing errors about values not being valid UTF8), but not this one.

---
### Prerequisites

This tool was written and tested with Go version 1.10.3, so I would recommend having at least that version when using this tool, otherwise you might experience some issues.

Go's installing instructions can be found here https://golang.org/doc/install#install

Once Go is installed, and you've added the go/bin folder to your path, you can install `smg-live-alter`.

```
go get github.com/StirlingMarketingGroup/smg-live-alter
go install smg-live-alter
smg-live-alter -help
---

### Usage

![helpful screenshot](https://d159l1kvshziji.cloudfront.net/i/Fg-/A.jpg)

```
./smg-live-alter -help
  -P int
        your MySQL port (default 3306)
  -d string
        your MySQL database
  -h string
        your MySQL host (default "localhost")
  -p string
        your MySQL password
  -prefix string
        the prefix of the new tmp table (default "_SMGLA_")
  -q string
        the alter table query to be executed
  -u string
        your MySQL username (default "root")
```

As you can see, there's not a lot of options here. Yay simplicity!

There’re two arguments I can see here that may need a tad explained, however:

1. `-prefix` - This is simply the prefix that this tool uses on the temp tables it generates. This should be something that won't collide with other table names. Example: if you have two tables, one named `orders` (that's the one being altered) and another table named `placedorders`, then don't set your prefix to "placed" because it will drop `placeorders` thinking it's a left-over temp table from a previous run.

2. `-q` - This is the alter statement. Paste it here in its entirety. an example would be something like this: "``ALTER TABLE `sterling`.`notifications` CHANGE COLUMN `Shown` `Shown` TINYINT(1) NOT NULL DEFAULT '0' ;``"
<sub>Tip: Make sure you remove the newlines before pasting, or the returns in the pasted statement will try to execute the command.</sub>

The tool parses the table name and other things from the alter query, so there's no need to give that as a separate option (looking at you two, GitHub and Percona).

---

### How it works

Unlike the ultra-fancy way that the GitHub tool works, this one works more closely to how the rest work; with triggers. Now the fact that this uses triggers means that it will **only work with MySQL version 5.7.2 and above**. Another requirement of this tool is that it **only works with tables that have primary keys**. This is due to how it chunks inserts, ordering it by the primary key (or multiple primary keys, this tool supports that, too), and then selecting the first primary key values from the temp table ordered by the primary keys in reverse to figure out what data to select next.

To avoid the problem of data being inserted in the middle of alter messing up what constitutes that max primary key values, inserted values go to a second table, to be inserted into the first table at the very end (we assume that they're aren't going to be problematic number of rows in this table in the time the alter took place).

Updates and deletions involving the main table are handled with triggers on that main table that apply the same update/delete to the data by primary key in both temp tables.

Once the data is all inserted into the first temp table;

1. The old table is dropped. This happens now to avoid consistency problems. We understand that dropping this table first and then doing other things before the first temp table is renamed will cause a very small amount of time that no table exists with the original table's name, but we took this tradeoff to ensure the data is as consistent as possible. Essentially, we require that the application using the table in production retries its queries if the table does not exist (something we were already doing, since we used the drop-swap method before with pt-online-schema-change)

2. Insert the rows from the second, inserts, temp table into the first, main, temp table

3. Restore the triggers to the main temp table. We do this before we rename it the original table's name as well because, as mentioned earlier, triggers are *ultra-important* for us, and we can't have the able being written to without our triggers, so we'd rather it not exists yet.

4. Rename the original temp table to match that of the altered table.

5. Restore constraints. We are creating the first (and second) temp tables without constraints because they don't take time to add (with foreign keys disabled), and their names are unique to the entire DB, so this way we don't have to worry about prefixing these as well, and then removing the prefixes later.

6. Drop the second temp table.

7. ???

8. Profit

And that's it! That's our SMG Live Alter table, hope you like it as much as I do.
