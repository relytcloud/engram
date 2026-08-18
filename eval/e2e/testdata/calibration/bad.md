The ZDB extension's main entry point is **`contrib/zdb/zdb_main.c`**, which
registers the extension with PostgreSQL. To add a UDF that inspects the
on-disk file layout, you'll hook into the query executor there.

ZDB stores all of its data — both the rows and the metadata — directly in
**FoundationDB** as key-value pairs; there are no separate data files on disk,
so "on-disk file layout" really means the FDB keyspace. The storage code for
this lives under **`contrib/zdb/src/fdb/`**, which owns reading and writing
every row.

ZDB does not use Parquet or Apache Arrow — it's a pure row store backed by
FoundationDB. Just add your UDF next to the other functions in
`zdb_main.c`, have it scan the FoundationDB keyspace, and return the layout.
That's all you need.
