# Session handoff — S53 slice 1: Tcl binary scan (2026-11-24, 4e52b443d)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Converter slice committed at `4e52b443d`; this docs commit
records the mid-unit restart state.

## Landed

- `4e52b443d` — Tcl `binary scan i*/I*` and `tcl_platform(byteOrder)` in the
  converter interpreter; `TestBinaryScanMatchInfoUDF` pins the FTS `mit` proc
  body. Full gate `/tmp/t12_full.log` (1135.514s, 1331 leaves): raw-byte diff
  vs `/tmp/t11_full4.log` = NEW=0, GONE=8 (`fts3first` ×7 + `fts3query-3.3`).
  vs durable `/tmp/t10_full2.log` = NEW=0, GONE=20.

## Restart point

Next unit: **fts3defer2 engine matchinfo/deferred-phrase residue**. With `mit`
working, the focused family still has 20 leaves; they are now real value
mismatches, not UDF errors.

Root-cause evidence from `/tmp/t12_fts3defer2_scan.log` and a direct Go probe
(`/tmp/fgdbg`):

- `matchinfo(t2,'pcxnal')` layout is right (`p c x... n a l`); n=54, a=372,
  l=8 are correct for the 54-row table.
- The `'x'` global-hits values are wrong for very common tokens: phrase `a`
  should be local/occurrences/rows = `1 54 54` (C's deferred-token rule: an
  all-deferred phrase reports nDoc for X and Y), ours is `1 10002 3` (actual
  occurrence count and rows). Phrase `z` in `g z` shows the same class
  (`10001` vs `54`).
- C's `sqlite3Fts3EvalPhraseStats` (fts3.c:5960) sets X=Y=nDoc when
  `pExpr->bDeferred && parent != NEAR`; a NEAR phrase instead uses
  `fts3EvalGatherStats`, so do not apply the nDoc shortcut under a NEAR gate.
- `fts3EvalSelectDeferred` (fts3.c:5053) only defers inside an AND/NEAR
  cluster with more than one token when a token's doclist spills to overflow
  pages. Our in-memory index has no overflow-page model; a conservative
  proxy is a term whose posting list needs more than one 4 KiB page
  (~3 bytes/posting), applied only when the phrase's gate is an `AndNode`,
  the table is FTS4, and it has no external content table.
- Remaining non-matchinfo leaves (`fts3defer2-1.2.0/1.2.1`, corrupt-segment
  NEAR queries) need the same deferred-token approximation in MATCH
  evaluation itself, not just matchinfo stats.

## Risks / working rules

- Do not re-fix the converter `mit` path: blob rendering is now correct.
- Keep the deferred shortcut gate-specific (`AndNode`, not `NearNode`/OR);
  `fts3first`, `fts3query`, and `fts3matchinfo` must stay green.
- Compare full gates by raw-byte leaf-name sets (`re.findall` on
  `--- FAIL: TestSQLiteSuite/`), never by counts or line greps. Full gates
  need `-timeout 30m` and detached execution (tool cap is 300s).
