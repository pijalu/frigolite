package fts5

// This file ports the module-init scalar functions of fts5_main.c's
// sqlite3Fts5Init (fts5Fts5Func:3577, fts5SourceIdFunc:3595): fts5(X) hands
// the extension API pointer to a host embedding through an
// sqlite3_value_pointer("fts5_api_ptr") argument, and fts5_source_id()
// reports the extension's source tag. The pure-Go engine has no C API
// pointer domain, so fts5(X) returns NULL for every SQL argument (C only
// writes through ppApi when the argument carries the pointer tag) and
// fts5_source_id() returns C's literal tag.

// APIFunc implements the SQL scalar fts5(X) (fts5_main.c fts5Fts5Func). C
// writes the fts5_api pointer through the argument only when it carries the
// "fts5_api_ptr" tag and otherwise leaves the result unset (NULL); no SQL
// value can carry that tag, so the observable result is always NULL.
func APIFunc(_ []interface{}) (interface{}, error) {
	return nil, nil
}

// SourceIDFunc implements fts5_source_id() (fts5_main.c fts5SourceIdFunc):
// the extension's fixed source tag, asserted zero-argument in C.
func SourceIDFunc(_ []interface{}) (interface{}, error) {
	return "--FTS5-SOURCE-ID--", nil
}
