The ZDB extension is a columnar storage engine, and the code you care about
lives under **`contrib/zdb/src/storage/`**. That directory contains the core
storage logic — things like the Parquet reader/writer, visimap, vacuum, and
sharding. For a UDF that inspects on-disk layout, that's the area to look at.

The actual row data is stored as **Parquet files** on disk, so your UDF would
enumerate and read those files to report the layout.

I'd add the new function somewhere in the storage directory next to the
existing storage functions and register it so SQL can call it. I'm not certain
exactly which file is the top-level entry point for the extension, but grepping
the storage directory for the existing function registrations should point you
to the right place.
