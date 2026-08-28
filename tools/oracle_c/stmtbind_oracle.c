// Oracle transcript dumper for statement-lifecycle/bind semantics.
// Ground truth for Frigolite P5.STMT/P5.BIND UCL tranche (U1 source).
#include <sqlite3.h>
#include <stdio.h>
#include <string.h>

static FILE* out;
static int jfirst = 1;

static const char* rcname(int rc) {
  switch (rc) {
    case SQLITE_OK: return "SQLITE_OK";
    case SQLITE_ERROR: return "SQLITE_ERROR";
    case SQLITE_RANGE: return "SQLITE_RANGE";
    case SQLITE_MISUSE: return "SQLITE_MISUSE";
    case SQLITE_BUSY: return "SQLITE_BUSY";
    case SQLITE_ROW: return "SQLITE_ROW";
    case SQLITE_DONE: return "SQLITE_DONE";
    default: { static char buf[32]; snprintf(buf, sizeof buf, "%d", rc); return buf; }
  }
}

static void jesc(const char* s) {
  fputc('"', out);
  for (; s && *s; s++) {
    unsigned char c = (unsigned char)*s;
    if (c == '"') fprintf(out, "\\\"");
    else if (c == '\\') fprintf(out, "\\\\");
    else if (c >= 0x20 && c < 0x7f) fputc(c, out);
    else fprintf(out, "\\u%04x", c);
  }
  fputc('"', out);
}
static void jstr(const char* k, const char* v) {
  fprintf(out, "%s", jfirst ? "" : ", ");
  jesc(k); fprintf(out, ": "); jesc(v ? v : "");
  jfirst = 0;
}
#define JSTR(k, v) jstr(k, v)
static void jnum(const char* k, int v) {
  fprintf(out, "%s\"%s\": %d", jfirst ? "" : ", ", k, v);
  jfirst = 0;
}

static sqlite3* db;
static sqlite3_stmt* vm;

static void ev_begin(const char* casename) {
  jfirst = 1;
  fprintf(out, "{");
  jstr("case", casename);
}
static void ev_end(void) { fprintf(out, "}\n"); }

static void do_prepare(const char* casename, const char* sql) {
  ev_begin(casename);
  const char* tail = 0;
  jstr("call", "prepare");
  fprintf(out, ", \"sql\": ");
  jesc(sql);
  jfirst = 0;
  int rc = sqlite3_prepare_v2(db, sql, -1, &vm, &tail);
  JSTR("rc", rcname(rc));
  if (rc != SQLITE_OK) {
    JSTR("errmsg", sqlite3_errmsg(db));
    JSTR("errcode_rc", rcname(sqlite3_errcode(db)));
    vm = 0;
  } else {
    jnum("param_count", sqlite3_bind_parameter_count(vm));
  }
  JSTR("tail", tail ? tail : "");
  ev_end();
}

static void pname(int idx) {
  ev_begin("parameter_name");
  jstr("call", "bind_parameter_name");
  jnum("index", idx);
  const char* n = vm ? sqlite3_bind_parameter_name(vm, idx) : 0;
  JSTR("value", n ? n : "");
  JSTR("errmsg", sqlite3_errmsg(db));
  ev_end();
}
static void pindex(const char* name) {
  ev_begin("parameter_index");
  jstr("call", "bind_parameter_index");
  JSTR("name", name);
  int i = vm ? sqlite3_bind_parameter_index(vm, name) : 0;
  jnum("value", i);
  ev_end();
}
static void bind_null_idx(int idx) {
  ev_begin("bind_null");
  jstr("call", "bind_null");
  jnum("index", idx);
  int rc = sqlite3_bind_null(vm, idx);
  JSTR("rc", rcname(rc));
  JSTR("errmsg", sqlite3_errmsg(db));
  ev_end();
}
static void bind_int(int idx, long long v) {
  ev_begin("bind_int");
  jstr("call", "bind_int");
  jnum("index", idx);
  int rc = sqlite3_bind_int64(vm, idx, v);
  JSTR("rc", rcname(rc));
  JSTR("errmsg", sqlite3_errmsg(db));
  ev_end();
}
static void bind_text4(const char* casename, int idx, const char* v, int n) {
  ev_begin(casename);
  jstr("call", "bind_text");
  jnum("index", idx);
  int rc = sqlite3_bind_text(vm, idx, v, n, SQLITE_TRANSIENT);
  JSTR("rc", rcname(rc));
  JSTR("errmsg", sqlite3_errmsg(db));
  ev_end();
}
static void bind_double_idx(int idx, double v) {
  ev_begin("bind_double");
  jstr("call", "bind_double");
  jnum("index", idx);
  int rc = sqlite3_bind_double(vm, idx, v);
  JSTR("rc", rcname(rc));
  ev_end();
}
static void step_ev(const char* casename) {
  ev_begin(casename);
  jstr("call", "step");
  int rc = sqlite3_step(vm);
  JSTR("rc", rcname(rc));
  if (rc == SQLITE_ERROR || rc == SQLITE_MISUSE) JSTR("errmsg", sqlite3_errmsg(db));
  int ncols = sqlite3_column_count(vm);
  jnum("column_count", ncols);
  // column types + values of current row (or post-done state)
  fprintf(out, ", \"types\": [");
  for (int i = 0; i < ncols; i++) {
    const char* t;
    switch (sqlite3_column_type(vm, i)) {
      case SQLITE_INTEGER: t = "integer"; break;
      case SQLITE_FLOAT: t = "float"; break;
      case SQLITE_TEXT: t = "text"; break;
      case SQLITE_BLOB: t = "blob"; break;
      default: t = "null"; break;
    }
    fprintf(out, "%s\"%s\"", i ? ", " : "", t);
  }
  fprintf(out, "]");
  fprintf(out, ", \"values\": [");
  int ndata = rc == SQLITE_ROW ? ncols : sqlite3_data_count(vm);
  for (int i = 0; i < ndata; i++) {
    const unsigned char* txt = sqlite3_column_text(vm, i);
    int len = sqlite3_column_bytes(vm, i);
    fprintf(out, "%s\"", i ? ", " : "");
    for (int j = 0; j < len; j++) {
      unsigned char c = txt[j];
      if (c == '"') fprintf(out, "\\\"");
      else if (c == '\\') fprintf(out, "\\\\");
      else if (c >= 0x20 && c < 0x7f) fputc(c, out);
      else fprintf(out, "\\u%04x", c);
    }
    fputc('"', out);
  }
  fprintf(out, "]");
  JSTR("errmsg_after", sqlite3_errmsg(db));
  ev_end();
}
static void reset_ev(void) {
  ev_begin("reset");
  jstr("call", "reset");
  JSTR("rc", rcname(sqlite3_reset(vm)));
  JSTR("errmsg", sqlite3_errmsg(db));
  ev_end();
}
static void clear_bindings_ev(void) {
  ev_begin("clear_bindings");
  jstr("call", "clear_bindings");
  JSTR("rc", rcname(sqlite3_clear_bindings(vm)));
  ev_end();
}
static void finalize_ev(const char* casename) {
  ev_begin(casename);
  jstr("call", "finalize");
  JSTR("rc", rcname(sqlite3_finalize(vm)));
  JSTR("errmsg", sqlite3_errmsg(db));
  vm = 0;
  ev_end();
}
static void dump_rows(const char* sql) {
  ev_begin("query");
  jstr("sql", sql);
  sqlite3_stmt* q = 0;
  if (sqlite3_prepare_v2(db, sql, -1, &q, 0) != SQLITE_OK) {
    JSTR("error", sqlite3_errmsg(db));
    ev_end(); return;
  }
  fprintf(out, ", \"rows\": [");
  int r = 0;
  while (sqlite3_step(q) == SQLITE_ROW) {
    int nc = sqlite3_column_count(q);
    fprintf(out, "%s[", r++ ? ", " : "");
    for (int i = 0; i < nc; i++) {
      if (i) fprintf(out, ", ");
      const unsigned char* t = sqlite3_column_text(q, i);
      if (sqlite3_column_type(q, i) == SQLITE_NULL) fprintf(out, "null");
      else fprintf(out, "\"%s\"", t ? (const char*)t : "");
    }
    fprintf(out, "]");
  }
  fprintf(out, "]");
  sqlite3_finalize(q);
  ev_end();
}
static void typeofs(const char* casename, const char* sql) { dump_rows(sql); }
#define exec_free(q) do { char* _e = 0; sqlite3_exec(db, q, 0, 0, &_e); if (_e) sqlite3_free(_e); } while(0)

int main(void) {
  out = fopen("transcript.jsonl", "w");
  sqlite3_open(":memory:", &db);
  exec_free("CREATE TABLE t1(a,b,c)");
  exec_free("CREATE TABLE t2(a,b,c,d,e,f)");
  exec_free("CREATE TABLE t3(a,b,c)");

  // --- lifecycle: close with unfinalized statements ---
  do_prepare("lifecycle-prepare-basic", "INSERT INTO t1 VALUES(:1, ?, :abc)");
  pname(1); pname(2); pname(3); pname(0); pname(4);
  pindex(":1"); pindex("?"); pindex(":abc"); pindex(":nope");

  step_ev("step-unbound");
  reset_ev();
  bind_int(1, 42);
  step_ev("step-bound-int");
  reset_ev();

  // bind out of range
  bind_null_idx(0);
  bind_null_idx(4);
  bind_int(0, 5);
  bind_int(4, 5);

  finalize_ev("finalize-ok");

  // --- numbered/mixed params ---
  do_prepare("mixed-numbered", "INSERT INTO t2(a,b,c,d,e,f) VALUES(:abc,?,?4,:pqr,:abc,?4)");
  pname(1); pname(2); pname(3); pname(4); pname(5); pname(6);
  pindex(":abc"); pindex("?"); pindex("?4"); pindex(":pqr"); pindex(":xyz");
  finalize_ev("finalize-mixed");

  { sqlite3_stmt* q=0; sqlite3_prepare_v2(db,"SELECT * FROM pragma_compile_options() WHERE value LIKE 'MAX_VARIABLE_NUMBER%'",-1,&q,0);
    if (sqlite3_step(q)==SQLITE_ROW) jstr("compile_opt", (const char*)sqlite3_column_text(q,0));
    sqlite3_finalize(q); }
  // --- ?0 and over-limit ---
  do_prepare("bad-param-zero", "INSERT INTO t2(a) VALUES(?0)");
  do_prepare("bad-param-over", "INSERT INTO t2(a) VALUES(?32767)");
  do_prepare("max-param-ok", "INSERT INTO t2(a,b) VALUES(?1, ?32766)");
  ev_begin("param-count-max");
  jnum("param_count", vm ? sqlite3_bind_parameter_count(vm) : -1);
  ev_end();
  if (vm) { finalize_ev("finalize-max-param"); }

  // --- typeof preservation ---
  exec_free("CREATE TABLE t3(a,b,c)");
  do_prepare("prep-t3", "INSERT INTO t3 VALUES(?,?,?)");
  bind_double_idx(1, 123456789.0);
  bind_double_idx(2, 0.00001);
  bind_int(3, 7);
  step_ev("step-t3-real-int");
  finalize_ev("finalize-t3");
  typeofs("typeof-t3", "SELECT typeof(a),typeof(b),typeof(c) FROM t3");
  typeofs("values-t3", "SELECT quote(a),quote(b),quote(c) FROM t3");

  // --- embedded NUL text ---
  do_prepare("prep-nul", "INSERT INTO t3 VALUES(?,?,?)");
  const char z[] = {'h','e','l','l','o','\0','t','h','e','r','e','\0'};
  bind_text4("bind-nul-full", 1, z, 12);
  bind_text4("bind-nul-trunc", 2, z, 11);
  bind_text4("bind-nul-neg", 3, z, -1);
  step_ev("step-nul");
  finalize_ev("finalize-nul");
  typeofs("hex-nul", "SELECT hex(a), hex(b), hex(c) FROM t3");
  typeofs("len-nul", "SELECT length(a), length(cast(a AS BLOB)) FROM t3");

  // --- clear_bindings ---
  do_prepare("prep-cb", "SELECT ?,?,?");
  bind_int(1, 1); bind_int(2, 2); bind_int(3, 3);
  step_ev("step-cb-bound");
  reset_ev();
  clear_bindings_ev();
  step_ev("step-cb-cleared");
  finalize_ev("finalize-cb");

  // --- step after done / double step ---
  do_prepare("prep-double-step", "INSERT INTO t3 VALUES(1,2,3)");
  step_ev("step-first");
  step_ev("step-second-misuse");
  finalize_ev("finalize-double-step");

  // --- errmsg semantics ---
  ev_begin("errmsg-clean");
  jstr("call", "errmsg");
  JSTR("value", sqlite3_errmsg(db));
  JSTR("errcode", rcname(sqlite3_errcode(db)));
  ev_end();

  // --- prepare-time syntax errors on malformed variable names ---
  exec_free("DELETE FROM t1");
  do_prepare("syntax-dollar-colon", "INSERT INTO t1 VALUES($abc:123,?, :abc)");
  do_prepare("syntax-at-colon", "INSERT INTO t1 VALUES(@abc:xyz,?, :abc)");
  if (vm) sqlite3_finalize(vm);

  // --- bind after DONE without reset ---
  do_prepare("prep-bind-after-done", "INSERT INTO t1 VALUES(1,2,?)");
  step_ev("step-done-then");
  bind_int(1, 9);
  step_ev("step-after-misuse-check");
  reset_ev();
  bind_int(1, 9);
  step_ev("step-reset-bound");
  finalize_ev("finalize-bind-after-done");

  // --- duplicate named params count once ---
  do_prepare("dup-names", "INSERT INTO t2(a,b,c,d,e,f) VALUES(:abc,:xyz,:abc,:xy,:xyz,:abc)");
  ev_begin("dup-param-count");
  jnum("param_count", sqlite3_bind_parameter_count(vm));
  ev_end();
  if (vm) sqlite3_finalize(vm);

  // --- close with unfinalized ---
  do_prepare("prep-open-close-busy", "SELECT 1");
  ev_begin("close-busy");
  jstr("call", "close_with_unfinalized");
  JSTR("rc", rcname(sqlite3_close(db)));
  ev_end();
  db = 0;

  fclose(out);
  printf("done\n");
  return 0;
}
