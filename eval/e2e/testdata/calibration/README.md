# Judge calibration fixtures

Three fixture answers for task **`arch-001`** (phoenix-e2e-v1) used to calibrate
the LLM judge before the dataset + `judge_prompt.md` freeze. Run with:

```bash
go run ./eval/cmd/evalrun -suite judge-calibrate -task arch-001
```

## Fixtures

- `good.md` — hits all five answer points with the correct file paths
  (`contrib/zdb/src/zdb.c` entry point, `storage/` + `storage/parquet/`
  organization, Parquet-via-Arrow row data, FDB/GMetaService metadata + NDP).
- `partial.md` — hits roughly half: names `storage/` and Parquet as the row
  format, but omits the entry point, the `storage/parquet/` breakdown, and the
  FDB/GMetaService/NDP metadata architecture, and hedges heavily.
- `bad.md` — confident but wrong: invents `zdb_main.c`, claims a pure
  FoundationDB key-value store, and denies any Parquet/Arrow usage.

## Recorded scores (model: sonnet)

| fixture  | score |
|----------|-------|
| good     | 10.0  |
| partial  | 3.0   |
| bad      | 0.0   |

Acceptance (`score(good) > score(partial) > score(bad)`, `good ≥ 7`,
`bad ≤ 3`) passed on the first run — no `judge_prompt.md` tuning was required.
