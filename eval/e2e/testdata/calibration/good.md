The ZDB extension's main entry point is **`contrib/zdb/src/zdb.c`** — that's
where the module registers its hooks and where you'd wire in a new UDF's
registration path.

The storage engine is organized under **`contrib/zdb/src/storage/`**, which
holds the core storage code: the Parquet reader/writer, catalog, visimap,
sharding, vacuum, delta, flush, auto-partition, and standby logic. Nested
inside it, **`contrib/zdb/src/storage/parquet/`** holds the Parquet-format
specifics — schema, data, encoding, statistics, and NDP file handling. For a
UDF that inspects on-disk file layout you'll most likely hook into the
`storage/parquet/` file-handling code to enumerate the Parquet files, and add
the SQL-facing function alongside the other functions registered from
`zdb.c`.

Two things to keep in mind about where data actually lives:

- The actual **row data is stored as Parquet files via Apache Arrow** — so
  "on-disk file layout" means the Parquet files produced through Arrow.
- The **metadata** (file lists, segment/shard bookkeeping — not the row data
  itself) lives in **FoundationDB via the GMetaService**, with **NDP used for
  caching / near-data processing**. So an inspection UDF will need to consult
  GMetaService/FDB for the metadata view of the layout, not just scan the
  Parquet files directly.
