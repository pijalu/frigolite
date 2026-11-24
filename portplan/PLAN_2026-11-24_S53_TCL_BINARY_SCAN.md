# Plan — S53 slice 1: Tcl binary scan for FTS matchinfo UDFs (2026-11-24)

Goal: make the JSON harness's Tcl callback UDFs render FTS4 `matchinfo()`
blobs the way tclsqlite does. Baseline: `/tmp/t11_full4.log` (1339 failing
leaves); focused `fts3defer2` baseline `/tmp/t12_fts3defer2_base.log`.

## Findings

`fts3defer2.test` registers `mit` as a Tcl proc:

```
set scan(littleEndian) i*
set scan(bigEndian) I*
binary scan $blob $scan($::tcl_platform(byteOrder)) r
return $r
```

The converter interpreter supported `binary encode hex` and `binary format
c*`, but not `binary scan`, so every `mit(...)` SQL function failed with
"unsupported binary form" before the engine's matchinfo value was compared.
This was a converter/harness fault, not an FTS engine bug: `hex(matchinfo())`
from a direct Go probe already produced a well-formed u32 blob.

## Changes

1. `tools/tclconvert/tcl/interp.go`: define the testfixture's
   `tcl_platform(byteOrder)` (`littleEndian`) in `NewInterp`.
2. `tools/tclconvert/tcl/interp_cmds.go`: handle `binary scan <data> <spec>
   <var>` for the FTS `i*`/`I*` native 32-bit integer fields; store the Tcl
   list in the result variable and return the conversion count.
3. `tools/tclconvert/tcl/interp_sql_test.go`: pin the exact `mit` proc body
   against a little-endian `[2 1 54]` matchinfo-shaped blob.

## Verification

- Unit `TestBinaryScanMatchInfoUDF` green; imposter unit still green.
- Full gate `/tmp/t12_full.log` (1135.514s, 1331 leaves): raw-byte diff vs
  `/tmp/t11_full4.log` = NEW=0, GONE=8 (`fts3first` ×7 + `fts3query-3.3`).
  vs durable `/tmp/t10_full2.log` = NEW=0, GONE=20 (those 8 plus S52's
  expridx1 leaves and the documented temptable2 flake).
- The remaining `fts3defer2` failures changed from UDF errors to real
  matchinfo value mismatches: deferred phrases report actual occurrence
  counts where C reports nDoc. That is the engine-side next unit.
