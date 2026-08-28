# TASK G6 — Extensions & Virtual Tables (JSON, RTREE, vtab modules, FTS3/4/5)

> **Phase**: G6 (depends on G5 core goals green)
> **Goal IDs**: G6.JSON, G6.RTREE, G6.VTAB-MODULES, G6.FTS3, G6.FTS5
> **Read first**: `PORTPLAN.md` §0 (principle #4: stdlib),
> **`portplan/DESIGN.md` §I (FTS5 architecture + FTS3/4 alias decision) + §J
> (JSON parser/JSONB, RTREE, vtab modules)**, `portplan/GUIDELINES.md`.
> **Status**: ⚪ not started
>
> **Directive**: These were all marked N/A as "not implemented". **Implement
> them.** The only exception is where Frigolite already has an equivalent-or-
> better implementation (none here yet). Use the SQLite `ext/` source as the
> reference; favor Go stdlib building blocks.

---

## Objective

Implement the JSON1 functions, the R*Tree virtual table, the loadable vtab
modules (csv, carray, intarray, series, nextchar, closure, spellfix, …), and the
full-text search engines (FTS3/4 then FTS5). These are the largest subsystems.

---

## Goal G6.JSON — JSON1 functions

**Scope**: `json101`–`json109`, `json501`, `json502`, `jsonb01`. (Un-skip `json`/
`jsonb` from `skipTestFiles`.)

**Key areas**: `internal/function/` (json funcs) + `internal/jsonb/` (new: JSON
parse + JSONB binary encoding). **stdlib**: hand-roll a small JSON parser per
SQLite's rules (path expressions `$.a.b[0]`, not Go `encoding/json` semantics —
SQLite's JSON path/typing differs), but reuse `encoding/json` for the core
tokenizer where compatible. Reference SQLite `src/json.c`, `ext/misc/json_*`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/json101/ ./testgen/json102/ ./testgen/jsonb01/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP6Json' -count=1 . && go build ./... && make quality
```

**Todos**:
1. Un-skip `json`/`jsonb`; scaffold `internal/jsonb/` (parser, JSONB codec,
   `->`/`->>` operators already lexed).
2. Functions: `json`, `json_array`, `json_object`, `json_array_length`,
   `json_extract`, `json_type`, `json_valid`, `json_quote`, `json_insert`,
   `json_replace`, `json_set`, `json_patch`, `json_remove`, `json_group_array`,
   `json_group_object`, `json_each` (table-valued), `json_tree` (table-valued),
   `json_type`, `json quotation`, JSONB variants.
3. Path expressions; type coercion (JSON ↔ SQL); NULL handling.
4. `->`/`->>` (already parse) → evaluate (currently return NULL).
5. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G6.RTREE — R*Tree spatial index virtual table

**Scope**: `rtree` (rtree1–rtree8, geopoly). (Un-skip from `skipTestFiles`.)

**Key areas**: new `internal/rtree/` (R-tree as a virtual-table module).
Reference SQLite `ext/rtree/rtree.c`, `ext/rtree/geopoly.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s ./testgen/rtree/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP6Rtree' -count=1 . && go build ./... && make quality
```

**Todos**:
1. Scaffold `internal/rtree/` as a vtab module (CREATE VIRTUAL TABLE … USING rtree).
2. R-tree insert/delete/contain/overlap query (node splitting); 1–5 dimensions.
3. Auxiliary functions (`rtreecheck`, `rtreenode`, `rtreedepth`, `rtreeaux`).
4. geopoly (optional sub-goal): polygon containment.
5. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G6.VTAB-MODULES — Loadable virtual-table modules

**Scope**: `csv01`, `carray01`/`carray02`, `intarray`, `series` (generate_series
exists — complete it), `nextchar`, `amatch1`, `spellfix`/`spellfix2`–`spellfix4`,
`closure`, `stmtvtab`, `tabfunc`, `unionvtab`, `zipfile`/`zipfile2`,
`dbstat`/`dbpage`.

**Key areas**: `internal/vtab/` (module registry — already exists with
`generate_series`). Add modules. Reference SQLite `ext/misc/*.c`,
`ext/misc/csv.c`, `ext/misc/carray.c`, etc. Favor stdlib (`archive/zip` for
zipfile, `csv` for csv).

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/csv01/ ./testgen/carray01/ ./testgen/carray02/ ./testgen/intarray/ \
  ./testgen/series/ ./testgen/nextchar/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP6Vtab' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `csv` (stdlib `csv` reader), `carray`/`intarray` (bind array → vtab), `series`
   (complete generate_series edge cases).
2. `nextchar`, `amatch` (approximate match), `closure` (transitive closure),
   `stmtvtab` (prepared-statement-backed vtab), `tabfunc` (table-valued funcs),
   `unionvtab` (union of tables).
3. `spellfix` (edit-distance phone-matching — `ext/misc/spellfix1.c` port).
4. `zipfile` (stdlib `archive/zip`), `dbstat`/`dbpage` (introspection vtabs).
5. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G6.FTS3 — Full-Text Search (FTS3/4)

**Scope**: `fts3aa`–`fts3an`, `fts3atoken`/`fts3atoken2`, `fts3aux1`/`fts3aux2`,
`fts3b`–`fts3f`, `fts3auto`, `fts3comp1`, `fts3conf`, `fts3cov`, `fts3defer`/
`defer2`/`defer3`, `fts3drop`/`dropmod`, `fts3expr`–`fts3expr5`, `fts3first`,
`fts3integrity`, `fts3matchinfo`, `fts3prefix`, `fts3snippet`, `fts3tok`/`tok_`,
`fts4growth`/`intck`/`merge`. (~94 FTS packages total; FTS3/4 subset here.)

**Key areas**: `internal/fts/` (already ~2000 lines: tokenizer, query, storage —
extend to full FTS3/4 semantics). Reference SQLite `ext/fts3/`.
**stdlib**: `unicode` for case-folding; `regexp` for token filters.

> **Note**: FTS5 is the modern engine SQLite recommends. FTS3/4 tests may share
> the tokenizer + inverted-index infrastructure. Decide (spike first) whether to
> implement FTS3/4 on its own or to make FTS3/4 queries route through the FTS5
> index where compatible. The legacy "FTS3/4/5 not implemented" skip is **not**
> acceptable; implement at least one engine that satisfies both where possible.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 200s \
  ./testgen/fts3aa/ ./testgen/fts3ab/ ./testgen/fts3ac/ ./testgen/fts3b/ \
  ./testgen/fts3c/ ./testgen/fts3d/ ./testgen/fts3e/ ./testgen/fts3f/ \
  2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP6Fts3' -count=1 . && go build ./... && make quality
```

**Todos**:
1. Spike: assess `internal/fts/` coverage; design shadow-table schema
   (`%_content`, `%_segments`, `%_segdir`, `%_stat`) matching SQLite on-disk.
2. Tokenizer: simple + porter + unicode61 (stdlib `unicode`).
3. MATCH query expression parser + ranking (bm25 / default).
4. INSERT/UPDATE/DELETE maintaining the inverted index; snippet/offsets/
   matchinfo auxiliary functions.
5. Un-skip FTS3/4 packages; regenerate; per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G6.FTS5 — Full-Text Search (FTS5)

**Scope**: `fts5`* packages (the FTS5 subset of the ~94 FTS packages),
`fts5aa`–`fts5`*, `fts5vocab`, `fts5 tokenizer/config/expr` packages.

**Key areas**: `internal/fts5/` (new). Reference SQLite `ext/fts5/`.
**stdlib**: `unicode`, `regexp`, `sort`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 200s ./testgen/fts5aa/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP6Fts5' -count=1 . && go build ./... && make quality
```

**Todos**:
1. FTS5 module: `CREATE VIRTUAL TABLE … USING fts5(...)`; shadow tables
   (`%_data`, `%_idx`, `%_content`, `%_config`); config parsing.
2. Tokenizer API (unicode61 default, ascii, porter, trigram); `fts5_tokenize`.
3. FTS5 query expression (AND/OR/NOT/phrase/NEAR, column filters, prefix).
4. Segment format (doclist, b-tree of terms); merge; ranking (bm25); the
   `fts5_*` auxiliary functions (highlight, snippet, bm25, rank).
5. `fts5vocab` virtual table; contentless/external-content/columnsize options.
6. Un-skip FTS5 packages; regenerate; per fix: pre-test + oracle → fix → verify → commit.

---

## Definition of Done (this task)
- All five goals green; pre-tests pass; quality + SOLID pass; no G1–G5 regression.
- `PORTPLAN.md` §5 G6 rows → 🟢.
