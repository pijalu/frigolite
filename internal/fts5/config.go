package fts5

import (
	"fmt"
	"strings"
)

// This file ports fts5_config.c's configuration parsing: the CREATE VIRTUAL
// TABLE argument list (columns + options) of the fts5 module. The accepted
// options and their error texts mirror fts5ConfigParse and
// fts5ConfigParseSpecial so CREATE statements fail with SQLite's messages.

// DetailMode mirrors Fts5Config.eDetail: how much per-instance information
// the index stores (fts5Int.h FTS5_DETAIL_*).
type DetailMode int

const (
	// DetailNone stores no per-instance column/position data (detail=none).
	DetailNone DetailMode = iota
	// DetailColumns stores the column but no positions (detail=columns).
	DetailColumns
	// DetailFull stores column + token positions (default).
	DetailFull
)

// ContentMode mirrors Fts5Config.eContent (FTS5_CONTENT_*).
type ContentMode int

const (
	// ContentNormal is the default: values stored in %_content.
	ContentNormal ContentMode = iota
	// ContentNone is content='' (contentless: no %_content shadow).
	ContentNone
	// ContentExternal is content=<table> (values read from the source table).
	ContentExternal
	// ContentUnindexed is contentless with contentless_unindexed=1 and at
	// least one UNINDEXED column (a %_content table holding those columns).
	ContentUnindexed
)

// FTS5MaxPrefixIndexes mirrors FTS5_MAX_PREFIX_INDEXES (fts5Int.h:96).
const FTS5MaxPrefixIndexes = 31

// Config is a parsed fts5 CREATE VIRTUAL TABLE configuration (Fts5Config).
type Config struct {
	// Name is the virtual table name (C's zName; zDb lives in Table.dbName).
	Name string
	// Columns lists the user columns in declared order.
	Columns []string
	// Unindexed[i] reports whether Columns[i] was declared UNINDEXED.
	Unindexed []bool

	// Prefix holds the prefix-index lengths (prefix= option).
	Prefix []int
	// TokSpec holds the whitespace-split tokenize= directive words
	// (tokenizer name + constructor arguments, dequoted).
	TokSpec []string

	// EContent is the content mode; ContentTable names the external content
	// table (content=<table>); ContentRowid is its rowid column.
	EContent     ContentMode
	ContentTable string
	ContentRowid string

	// ContentlessDelete / ContentlessUnindexed mirror the same-named options;
	// ColumnSize is columnsize=1 (default); Detail the detail= mode; Locale
	// and Tokendata the corresponding flags.
	ContentlessDelete    bool
	ContentlessUnindexed bool
	ColumnSize           bool
	Detail               DetailMode
	Locale               bool
	Tokendata            bool

	// Rank is the resolved rank-function configuration (the 'rank' special
	// insert: C's pConfig->zRank/zRankArgs; empty Func means the default
	// "bm25" with no arguments).
	Rank RankSpec
}

// Contentless reports whether the table is content=” (no stored text).
func (c *Config) Contentless() bool {
	return c.EContent == ContentNone || c.EContent == ContentUnindexed
}

// DetailFull reports whether per-instance positions are stored.
func (c *Config) DetailFull() bool { return c.Detail == DetailFull }

// ParseConfig parses the CREATE VIRTUAL TABLE argument list into a Config
// (fts5_config.c sqlite3Fts5ConfigParse). args are the module arguments after
// the table name (each column or key=value option); the frigolite vtab layer
// supplies the table name separately instead of C's azArg[2]. A zero-column
// configuration fails like C's failed sqlite3_declare_vtab.
func ParseConfig(name string, args []string) (*Config, error) {
	cfg := &Config{
		Name:       name,
		ColumnSize: true,
		Detail:     DetailFull,
	}
	if strings.EqualFold(name, "rank") {
		return nil, fmt.Errorf("reserved fts5 table name: %s", name)
	}
	for _, arg := range args {
		if arg == "" {
			continue // defensive: an empty module argument list entry
		}
		key, val, isOption, err := splitConfigArg(arg)
		if err != nil {
			return nil, err
		}
		if isOption {
			if err := parseSpecial(cfg, key, val); err != nil {
				return nil, err
			}
			continue
		}
		if err := parseColumn(cfg, key, val); err != nil {
			return nil, err
		}
	}
	if err := finishConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// finishConfig applies the cross-option constraints and defaults
// (fts5ConfigParse tail). The zero-column rejection lives with the table-name
// checks (Bind), where the resolved name is known for C's constructor error.
func finishConfig(cfg *Config) error {
	if cfg.ContentlessDelete && !cfg.Contentless() {
		return fmt.Errorf("contentless_delete=1 requires a contentless table")
	}
	if cfg.ContentlessDelete && !cfg.ColumnSize {
		return fmt.Errorf("contentless_delete=1 is incompatible with columnsize=0")
	}
	if cfg.ContentlessUnindexed && !cfg.Contentless() {
		return fmt.Errorf("contentless_unindexed=1 requires a contentless table")
	}
	if cfg.EContent != ContentNone && cfg.ContentTable == "" {
		// C fills zContent with the default shadow (%_content/%_docsize); the
		// Go storage layer derives shadow names from the mode, so only the
		// contentless_unindexed promotion happens here.
		if len(cfg.Unindexed) > 0 && cfg.ContentlessUnindexed {
			cfg.EContent = ContentUnindexed
		}
	}
	if cfg.ContentRowid == "" {
		cfg.ContentRowid = "rowid"
	}
	return nil
}

// splitConfigArg gobbles one argument into (key, value, isOption)
// (fts5ConfigParse's GobbleWord/SkipWhitespace dance): "col", "col UNINDEXED",
// "option=value". A quoted key forces a column ("parse error in ..." when a
// quoted word is followed by '=').
func splitConfigArg(arg string) (key, val string, isOption bool, err error) {
	rest, k, quoted, ok := gobbleWord(arg)
	if !ok {
		return "", "", false, fmt.Errorf("parse error in \"%s\"", arg)
	}
	rest = skipWhitespace(rest)
	if strings.HasPrefix(rest, "=") {
		if quoted {
			return "", "", false, fmt.Errorf("parse error in \"%s\"", arg)
		}
		rest = rest[1:]
		rest = skipWhitespace(rest)
		rest, v, _, ok := gobbleWord(rest)
		if !ok {
			return "", "", false, fmt.Errorf("parse error in \"%s\"", arg)
		}
		rest = skipWhitespace(rest)
		if rest != "" {
			return "", "", false, fmt.Errorf("parse error in \"%s\"", arg)
		}
		return k, v, true, nil
	}
	rest = skipWhitespace(rest)
	if rest != "" {
		rest2, v, _, ok := gobbleWord(rest)
		if !ok {
			return "", "", false, fmt.Errorf("parse error in \"%s\"", arg)
		}
		rest2 = skipWhitespace(rest2)
		if rest2 != "" {
			return "", "", false, fmt.Errorf("parse error in \"%s\"", arg)
		}
		return k, v, false, nil
	}
	_ = quoted
	return k, "", false, nil
}

// gobbleWord consumes one bare or quoted word (fts5ConfigGobbleWord +
// fts5Dequote). It returns the remaining text, the dequoted word, whether the
// word was quoted, and success (an unterminated quote fails).
func gobbleWord(s string) (rest, word string, quoted bool, ok bool) {
	if s == "" {
		return "", "", false, false
	}
	switch s[0] {
	case '\'', '"', '`', '[':
		q := s[0]
		closeQ := q
		if q == '[' {
			closeQ = ']'
		}
		var b strings.Builder
		i := 1
		for i < len(s) {
			if s[i] == closeQ {
				if q != '[' && i+1 < len(s) && s[i+1] == q {
					// A doubled quote is an escaped quote.
					b.WriteByte(q)
					i += 2
					continue
				}
				return s[i+1:], b.String(), true, true
			}
			b.WriteByte(s[i])
			i++
		}
		return "", "", false, false
	default:
		i := 0
		for i < len(s) && !isBarewordEnd(s[i]) {
			i++
		}
		if i == 0 {
			return "", "", false, false
		}
		return s[i:], s[:i], false, true
	}
}

// isBarewordEnd reports whether byte b terminates a bareword (fts5_isopenquote
// / whitespace / '=' logic inverted: a bareword ends at whitespace, a quote
// start, '=', or a query-ish punctuation that cannot appear in a column name).
func isBarewordEnd(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f', '=', '\'', '"', '`', '[':
		return true
	}
	return false
}

// skipWhitespace skips a run of ASCII whitespace (fts5ConfigSkipWhitespace).
func skipWhitespace(s string) string {
	for len(s) > 0 {
		switch s[0] {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			s = s[1:]
			continue
		}
		return s
	}
	return s
}

// parseColumn handles one column declaration (fts5ConfigParseColumn): the
// optional second word must be UNINDEXED; rank/rowid are reserved.
func parseColumn(cfg *Config, col, arg string) error {
	if strings.EqualFold(col, "rank") || strings.EqualFold(col, "rowid") {
		return fmt.Errorf("reserved fts5 column name: %s", col)
	}
	unindexed := false
	if arg != "" {
		if strings.EqualFold(arg, "unindexed") {
			unindexed = true
		} else {
			return fmt.Errorf("unrecognized column option: %s", arg)
		}
	}
	cfg.Columns = append(cfg.Columns, col)
	cfg.Unindexed = append(cfg.Unindexed, unindexed)
	return nil
}

// parseSpecial dispatches one key=value option (fts5ConfigParseSpecial).
func parseSpecial(cfg *Config, key, val string) error {
	switch {
	case strings.EqualFold(key, "prefix"):
		return parsePrefix(cfg, val)
	case strings.EqualFold(key, "tokenize"):
		return parseTokenize(cfg, val)
	case strings.EqualFold(key, "content"):
		if cfg.EContent != ContentNormal {
			return fmt.Errorf("multiple content=... directives")
		}
		if val != "" {
			cfg.EContent = ContentExternal
			cfg.ContentTable = val
		} else {
			cfg.EContent = ContentNone
		}
		return nil
	case strings.EqualFold(key, "contentless_delete"):
		b, err := flagArg(val, "contentless_delete")
		if err != nil {
			return err
		}
		cfg.ContentlessDelete = b
		return nil
	case strings.EqualFold(key, "contentless_unindexed"):
		b, err := flagArg(val, "contentless_delete")
		if err != nil {
			return err
		}
		cfg.ContentlessUnindexed = b
		return nil
	case strings.EqualFold(key, "content_rowid"):
		if cfg.ContentRowid != "" {
			return fmt.Errorf("multiple content_rowid=... directives")
		}
		cfg.ContentRowid = val
		return nil
	case strings.EqualFold(key, "columnsize"):
		b, err := flagArg(val, "columnsize")
		if err != nil {
			return err
		}
		cfg.ColumnSize = b
		return nil
	case strings.EqualFold(key, "locale"):
		b, err := flagArg(val, "locale")
		if err != nil {
			return err
		}
		cfg.Locale = b
		return nil
	case strings.EqualFold(key, "detail"):
		mode, ok := detailEnum(val)
		if !ok {
			return fmt.Errorf("malformed detail=... directive")
		}
		cfg.Detail = mode
		return nil
	case strings.EqualFold(key, "tokendata"):
		b, err := flagArg(val, "tokendata")
		if err != nil {
			return err
		}
		cfg.Tokendata = b
		return nil
	}
	return fmt.Errorf("unrecognized option: \"%s\"", key)
}

// flagArg parses a 0/1 flag value; the C error text reuses the
// contentless_delete wording for contentless_unindexed (fts5ConfigParseSpecial
// copy-through), which callers preserve by passing that directive name there.
func flagArg(val, directive string) (bool, error) {
	if (val == "0" || val == "1") && len(val) == 1 {
		return val == "1", nil
	}
	return false, fmt.Errorf("malformed %s=... directive", directive)
}

// detailEnum resolves the detail= enum with C's prefix matching
// (fts5ConfigSetEnum compares zArg as a prefix of each name).
func detailEnum(val string) (DetailMode, bool) {
	if val == "" {
		return DetailNone, false
	}
	for _, e := range []struct {
		name string
		mode DetailMode
	}{
		{"none", DetailNone},
		{"full", DetailFull},
		{"columns", DetailColumns},
	} {
		if strings.HasPrefix(e.name, val) {
			return e.mode, true
		}
	}
	return DetailNone, false
}

// parsePrefix parses the prefix= directive (fts5ConfigParseSpecial's prefix
// branch): a comma/space separated list of lengths 1..999.
func parsePrefix(cfg *Config, val string) error {
	p := val
	first := true
	for {
		p = skipWhitespace(p)
		if !first && strings.HasPrefix(p, ",") {
			p = skipWhitespace(p[1:])
		} else if p == "" {
			break
		}
		first = false
		if p == "" || p[0] < '0' || p[0] > '9' {
			return fmt.Errorf("malformed prefix=... directive")
		}
		if len(cfg.Prefix) >= FTS5MaxPrefixIndexes {
			return fmt.Errorf("too many prefix indexes (max %d)", FTS5MaxPrefixIndexes)
		}
		i := 0
		nPre := 0
		for i < len(p) && p[i] >= '0' && p[i] <= '9' && nPre < 1000 {
			nPre = nPre*10 + int(p[i]-'0')
			i++
		}
		p = p[i:]
		if nPre <= 0 || nPre >= 1000 {
			return fmt.Errorf("prefix length out of range (max 999)")
		}
		cfg.Prefix = append(cfg.Prefix, nPre)
	}
	return nil
}

// parseTokenize parses the tokenize= directive value into whitespace-separated
// words (fts5ConfigParseSpecial's tokenize branch): bare or quoted, each
// dequoted. Only one tokenize directive is allowed per table.
func parseTokenize(cfg *Config, val string) error {
	if cfg.TokSpec != nil {
		return fmt.Errorf("multiple tokenize=... directives")
	}
	p := val
	for p != "" {
		p = skipWhitespace(p)
		if p == "" {
			break
		}
		rest, word, _, ok := gobbleWord(p)
		if !ok {
			return fmt.Errorf("parse error in tokenize directive")
		}
		cfg.TokSpec = append(cfg.TokSpec, word)
		p = rest
	}
	if len(cfg.TokSpec) == 0 {
		return fmt.Errorf("parse error in tokenize directive")
	}
	return nil
}
