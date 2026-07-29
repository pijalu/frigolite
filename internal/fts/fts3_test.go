package fts

import (
	"testing"
)

// --- Tokenizer tests ---

func TestSimpleTokenizer(t *testing.T) {
	tok := &SimpleTokenizer{}

	tests := []struct {
		input string
		want  []string
	}{
		{"hello world", []string{"hello", "world"}},
		{"Hello World", []string{"hello", "world"}},
		{"hello, world!", []string{"hello", "world"}},
		{"  spaced  ", []string{"spaced"}},
		{"hello123", []string{"hello123"}},
		{"", nil},
		{"a b c", []string{"a", "b", "c"}},
	}

	for _, tt := range tests {
		tokens := tok.Tokenize(tt.input)
		got := make([]string, len(tokens))
		for i, tok := range tokens {
			got[i] = tok.Term
		}
		if len(got) != len(tt.want) {
			t.Errorf("Tokenize(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("Tokenize(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestSimpleTokenizerPositions(t *testing.T) {
	tok := &SimpleTokenizer{}
	tokens := tok.Tokenize("hello world foo")
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(tokens))
	}
	if tokens[0].Position != 0 {
		t.Errorf("token 0 position = %d, want 0", tokens[0].Position)
	}
	if tokens[1].Position != 1 {
		t.Errorf("token 1 position = %d, want 1", tokens[1].Position)
	}
	if tokens[2].Position != 2 {
		t.Errorf("token 2 position = %d, want 2", tokens[2].Position)
	}
}

func TestUnicode61Tokenizer(t *testing.T) {
	tok := &Unicode61Tokenizer{}

	tests := []struct {
		input string
		want  []string
	}{
		{"hello world", []string{"hello", "world"}},
		{"café", []string{"café"}},
		{"über cool", []string{"über", "cool"}},
		{"", nil},
	}

	for _, tt := range tests {
		tokens := tok.Tokenize(tt.input)
		got := make([]string, len(tokens))
		for i, tok := range tokens {
			got[i] = tok.Term
		}
		if len(got) != len(tt.want) {
			t.Errorf("Tokenize(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("Tokenize(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

// --- InvertedIndex tests ---

func TestInvertedIndexInsertAndSearch(t *testing.T) {
	idx := NewInvertedIndex()
	tok := &SimpleTokenizer{}

	id1 := idx.Insert([]string{"hello world"}, tok)
	if id1 != 1 {
		t.Errorf("first insert docid = %d, want 1", id1)
	}

	id2 := idx.Insert([]string{"goodbye world"}, tok)
	if id2 != 2 {
		t.Errorf("second insert docid = %d, want 2", id2)
	}

	// Search for "hello" - should find doc 1
	results := idx.SearchTerm("hello")
	if len(results) != 1 || results[0] != 1 {
		t.Errorf("SearchTerm('hello') = %v, want [1]", results)
	}

	// Search for "world" - should find docs 1 and 2
	results = idx.SearchTerm("world")
	if len(results) != 2 {
		t.Errorf("SearchTerm('world') = %v, want [1 2]", results)
	}

	// Search for "goodbye" - should find doc 2
	results = idx.SearchTerm("goodbye")
	if len(results) != 1 || results[0] != 2 {
		t.Errorf("SearchTerm('goodbye') = %v, want [2]", results)
	}

	// Search for nonexistent term
	results = idx.SearchTerm("nonexistent")
	if len(results) != 0 {
		t.Errorf("SearchTerm('nonexistent') = %v, want []", results)
	}
}

func TestInvertedIndexDelete(t *testing.T) {
	idx := NewInvertedIndex()
	tok := &SimpleTokenizer{}

	idx.Insert([]string{"hello world"}, tok)
	idx.Insert([]string{"goodbye world"}, tok)

	// Delete doc 1
	idx.Delete(1)

	// Search for "hello" - should find nothing
	results := idx.SearchTerm("hello")
	if len(results) != 0 {
		t.Errorf("after delete, SearchTerm('hello') = %v, want []", results)
	}

	// Search for "world" - should find doc 2 only
	results = idx.SearchTerm("world")
	if len(results) != 1 || results[0] != 2 {
		t.Errorf("after delete, SearchTerm('world') = %v, want [2]", results)
	}

	// Doc count should be 1
	if idx.DocCount() != 1 {
		t.Errorf("DocCount() = %d, want 1", idx.DocCount())
	}
}

func TestInvertedIndexUpdate(t *testing.T) {
	idx := NewInvertedIndex()
	tok := &SimpleTokenizer{}

	idx.Insert([]string{"hello world"}, tok)

	// Update doc 1
	idx.Update(1, []string{"foo bar"}, tok)

	// Should not find "hello" anymore
	results := idx.SearchTerm("hello")
	if len(results) != 0 {
		t.Errorf("after update, SearchTerm('hello') = %v, want []", results)
	}

	// Should find "foo"
	results = idx.SearchTerm("foo")
	if len(results) != 1 || results[0] != 1 {
		t.Errorf("after update, SearchTerm('foo') = %v, want [1]", results)
	}

	// Content should be updated
	doc := idx.GetDoc(1)
	if doc == nil || doc.Columns[0] != "foo bar" {
		t.Errorf("after update, doc content = %q, want %q", doc.Columns[0], "foo bar")
	}
}

func TestInvertedIndexPhrase(t *testing.T) {
	idx := NewInvertedIndex()
	tok := &SimpleTokenizer{}

	idx.Insert([]string{"hello world foo"}, tok)
	idx.Insert([]string{"hello foo world"}, tok)

	// Phrase "hello world" - should match doc 1 only
	results := idx.SearchPhrase([]string{"hello", "world"})
	if len(results) != 1 || results[0] != 1 {
		t.Errorf("SearchPhrase('hello world') = %v, want [1]", results)
	}

	// Phrase "world foo" - should match doc 1 only
	results = idx.SearchPhrase([]string{"world", "foo"})
	if len(results) != 1 || results[0] != 1 {
		t.Errorf("SearchPhrase('world foo') = %v, want [1]", results)
	}
}

func TestInvertedIndexPrefix(t *testing.T) {
	idx := NewInvertedIndex()
	tok := &SimpleTokenizer{}

	idx.Insert([]string{"hello world"}, tok)
	idx.Insert([]string{"help me"}, tok)
	idx.Insert([]string{"goodbye"}, tok)

	// Prefix "hel" - should match docs 1 and 2
	results := idx.SearchPrefix("hel")
	if len(results) != 2 {
		t.Errorf("SearchPrefix('hel') = %v, want [1 2]", results)
	}

	// Prefix "he" - should match docs 1 and 2
	results = idx.SearchPrefix("he")
	if len(results) != 2 {
		t.Errorf("SearchPrefix('he') = %v, want [1 2]", results)
	}

	// Prefix "good" - should match doc 3
	results = idx.SearchPrefix("good")
	if len(results) != 1 || results[0] != 3 {
		t.Errorf("SearchPrefix('good') = %v, want [3]", results)
	}

	// Prefix "xyz" - should match nothing
	results = idx.SearchPrefix("xyz")
	if len(results) != 0 {
		t.Errorf("SearchPrefix('xyz') = %v, want []", results)
	}
}

func TestInvertedIndexInsertWithID(t *testing.T) {
	idx := NewInvertedIndex()
	tok := &SimpleTokenizer{}

	idx.InsertWithID(100, []string{"hello"}, tok)
	idx.InsertWithID(200, []string{"world"}, tok)

	if idx.DocCount() != 2 {
		t.Errorf("DocCount() = %d, want 2", idx.DocCount())
	}

	doc := idx.GetDoc(100)
	if doc == nil || doc.Columns[0] != "hello" {
		t.Errorf("doc 100 content = %q, want %q", doc.Columns[0], "hello")
	}

	// Search should work with custom IDs
	results := idx.SearchTerm("hello")
	if len(results) != 1 || results[0] != 100 {
		t.Errorf("SearchTerm('hello') = %v, want [100]", results)
	}
}

// --- QueryParser tests ---

func TestParseTerm(t *testing.T) {
	node, err := ParseMatchQuery("hello")
	if err != nil {
		t.Fatalf("ParseMatchQuery('hello') error: %v", err)
	}
	_, ok := node.(*TermNode)
	if !ok {
		t.Fatalf("expected TermNode, got %T", node)
	}
}

func TestParsePhrase(t *testing.T) {
	node, err := ParseMatchQuery(`"hello world"`)
	if err != nil {
		t.Fatalf("ParseMatchQuery error: %v", err)
	}
	phrase, ok := node.(*PhraseNode)
	if !ok {
		t.Fatalf("expected PhraseNode, got %T", node)
	}
	if len(phrase.Terms) != 2 || phrase.Terms[0] != "hello" || phrase.Terms[1] != "world" {
		t.Errorf("phrase terms = %v, want [hello world]", phrase.Terms)
	}
}

func TestParsePrefix(t *testing.T) {
	node, err := ParseMatchQuery("hel*")
	if err != nil {
		t.Fatalf("ParseMatchQuery error: %v", err)
	}
	prefix, ok := node.(*PrefixNode)
	if !ok {
		t.Fatalf("expected PrefixNode, got %T", node)
	}
	if prefix.Prefix != "hel" {
		t.Errorf("prefix = %q, want %q", prefix.Prefix, "hel")
	}
}

func TestParseAND(t *testing.T) {
	node, err := ParseMatchQuery("hello AND world")
	if err != nil {
		t.Fatalf("ParseMatchQuery error: %v", err)
	}
	and, ok := node.(*AndNode)
	if !ok {
		t.Fatalf("expected AndNode, got %T", node)
	}
	if _, ok := and.Left.(*TermNode); !ok {
		t.Errorf("left should be TermNode")
	}
	if _, ok := and.Right.(*TermNode); !ok {
		t.Errorf("right should be TermNode")
	}
}

func TestParseImplicitAND(t *testing.T) {
	node, err := ParseMatchQuery("hello world")
	if err != nil {
		t.Fatalf("ParseMatchQuery error: %v", err)
	}
	_, ok := node.(*AndNode)
	if !ok {
		t.Fatalf("expected AndNode for implicit AND, got %T", node)
	}
}

func TestParseOR(t *testing.T) {
	node, err := ParseMatchQuery("hello OR goodbye")
	if err != nil {
		t.Fatalf("ParseMatchQuery error: %v", err)
	}
	or, ok := node.(*OrNode)
	if !ok {
		t.Fatalf("expected OrNode, got %T", node)
	}
	leftTerm, ok := or.Left.(*TermNode)
	if !ok || leftTerm.Term != "hello" {
		t.Errorf("left term = %v, want TermNode{hello}", or.Left)
	}
	rightTerm, ok := or.Right.(*TermNode)
	if !ok || rightTerm.Term != "goodbye" {
		t.Errorf("right term = %v, want TermNode{goodbye}", or.Right)
	}
}

func TestParseNOT(t *testing.T) {
	tests := []struct {
		query string
		desc  string
	}{
		{"hello -world", "minus syntax"},
		{"hello NOT world", "NOT keyword"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			node, err := ParseMatchQuery(tt.query)
			if err != nil {
				t.Fatalf("ParseMatchQuery(%q) error: %v", tt.query, err)
			}
			and, ok := node.(*AndNode)
			if !ok {
				t.Fatalf("expected AndNode for %q, got %T", tt.query, node)
			}
			notNode, ok := and.Right.(*NotNode)
			if !ok {
				t.Fatalf("expected NotNode on right for %q, got %T", tt.query, and.Right)
			}
			term, ok := notNode.Inner.(*TermNode)
			if !ok || term.Term != "world" {
				t.Errorf("NOT inner term = %v, want TermNode{world}", notNode.Inner)
			}
		})
	}
}

func TestParseColumnFilter(t *testing.T) {
	node, err := ParseMatchQuery("title:hello")
	if err != nil {
		t.Fatalf("ParseMatchQuery error: %v", err)
	}
	colRef, ok := node.(*ColumnRefNode)
	if !ok {
		t.Fatalf("expected ColumnRefNode, got %T", node)
	}
	if colRef.ColumnName != "title" {
		t.Errorf("column name = %q, want %q", colRef.ColumnName, "title")
	}
	term, ok := colRef.Inner.(*TermNode)
	if !ok || term.Term != "hello" {
		t.Errorf("inner = %v, want TermNode{hello}", colRef.Inner)
	}
}

func TestParseComplexQuery(t *testing.T) {
	// "one OR two" -> OrNode
	node, err := ParseMatchQuery("one OR two")
	if err != nil {
		t.Fatalf("ParseMatchQuery error: %v", err)
	}
	if _, ok := node.(*OrNode); !ok {
		t.Errorf("expected OrNode for 'one OR two', got %T", node)
	}

	// "one two three" -> And(And(one, two), three)
	node, err = ParseMatchQuery("one two three")
	if err != nil {
		t.Fatalf("ParseMatchQuery error: %v", err)
	}
	if _, ok := node.(*AndNode); !ok {
		t.Errorf("expected AndNode for 'one two three', got %T", node)
	}

	// "one OR two three" -> Or(one, And(two, three)) — OR has lower precedence
	node, err = ParseMatchQuery("one OR two three")
	if err != nil {
		t.Fatalf("ParseMatchQuery error: %v", err)
	}
	if _, ok := node.(*OrNode); !ok {
		t.Errorf("expected OrNode for 'one OR two three' (OR lower precedence), got %T", node)
	}
}

// --- QueryNode match tests ---

func TestQueryMatching(t *testing.T) {
	idx := NewInvertedIndex()
	tok := &SimpleTokenizer{}

	idx.Insert([]string{"hello world foo"}, tok)
	idx.Insert([]string{"goodbye world"}, tok)
	idx.Insert([]string{"hello there"}, tok)

	// Test TermNode
	term := &TermNode{Term: "hello"}
	if !term.MatchDoc(idx, 1) {
		t.Errorf("doc 1 should match 'hello'")
	}
	if term.MatchDoc(idx, 2) {
		t.Errorf("doc 2 should NOT match 'hello'")
	}
	if !term.MatchDoc(idx, 3) {
		t.Errorf("doc 3 should match 'hello'")
	}

	// Test AND
	and := &AndNode{Left: &TermNode{Term: "hello"}, Right: &TermNode{Term: "foo"}}
	if !and.MatchDoc(idx, 1) {
		t.Errorf("doc 1 should match 'hello AND foo'")
	}
	if and.MatchDoc(idx, 3) {
		t.Errorf("doc 3 should NOT match 'hello AND foo'")
	}

	// Test OR
	or := &OrNode{Left: &TermNode{Term: "hello"}, Right: &TermNode{Term: "goodbye"}}
	if !or.MatchDoc(idx, 1) {
		t.Errorf("doc 1 should match 'hello OR goodbye'")
	}
	if !or.MatchDoc(idx, 2) {
		t.Errorf("doc 2 should match 'hello OR goodbye'")
	}
	if !or.MatchDoc(idx, 3) {
		t.Errorf("doc 3 should match 'hello OR goodbye'")
	}

	// Test NOT
	not := &NotNode{Inner: &TermNode{Term: "world"}}
	if !not.MatchDoc(idx, 3) {
		t.Errorf("doc 3 should match 'NOT world'")
	}
	if not.MatchDoc(idx, 1) {
		t.Errorf("doc 1 should NOT match 'NOT world' (has world)")
	}
}

func TestPhraseMatching(t *testing.T) {
	idx := NewInvertedIndex()
	tok := &SimpleTokenizer{}

	idx.Insert([]string{"hello world foo"}, tok)
	idx.Insert([]string{"hello foo world"}, tok)

	phrase := &PhraseNode{Terms: []string{"hello", "world"}}
	if !phrase.MatchDoc(idx, 1) {
		t.Errorf("doc 1 should match phrase 'hello world'")
	}
	if phrase.MatchDoc(idx, 2) {
		t.Errorf("doc 2 should NOT match phrase 'hello world' (hello foo world)")
	}
}

func TestPrefixMatching(t *testing.T) {
	idx := NewInvertedIndex()
	tok := &SimpleTokenizer{}

	idx.Insert([]string{"hello"}, tok)
	idx.Insert([]string{"help"}, tok)
	idx.Insert([]string{"world"}, tok)

	prefix := &PrefixNode{Prefix: "hel"}
	if !prefix.MatchDoc(idx, 1) {
		t.Errorf("doc 1 should match prefix 'hel'")
	}
	if !prefix.MatchDoc(idx, 2) {
		t.Errorf("doc 2 should match prefix 'hel'")
	}
	if prefix.MatchDoc(idx, 3) {
		t.Errorf("doc 3 should NOT match prefix 'hel'")
	}
}

// --- FTS3Table tests ---

func TestFTS3TableCreate(t *testing.T) {
	table, err := NewFTS3Table("test", "fts3", []string{"content"})
	if err != nil {
		t.Fatalf("NewFTS3Table error: %v", err)
	}
	if table == nil {
		t.Fatal("table is nil")
	}
	names := table.ColumnNames()
	if len(names) != 1 || names[0] != "content" {
		t.Errorf("ColumnNames() = %v, want [content]", names)
	}
}

func TestFTS3TableDefaultColumn(t *testing.T) {
	table, err := NewFTS3Table("test", "fts3", []string{})
	if err != nil {
		t.Fatalf("NewFTS3Table error: %v", err)
	}
	names := table.ColumnNames()
	if len(names) != 1 || names[0] != "content" {
		t.Errorf("ColumnNames() = %v, want [content]", names)
	}
}

func TestFTS3TableMultiColumn(t *testing.T) {
	table, err := NewFTS3Table("test", "fts3", []string{"title", "body"})
	if err != nil {
		t.Fatalf("NewFTS3Table error: %v", err)
	}
	names := table.ColumnNames()
	if len(names) != 2 || names[0] != "title" || names[1] != "body" {
		t.Errorf("ColumnNames() = %v, want [title body]", names)
	}
}

func TestFTS3TableInsertAndMatch(t *testing.T) {
	table, _ := NewFTS3Table("test", "fts3", []string{"content"})

	id1 := table.Insert([]interface{}{"hello world"})
	if id1 != 1 {
		t.Errorf("insert returned %d, want 1", id1)
	}

	id2 := table.Insert([]interface{}{"goodbye world"})
	if id2 != 2 {
		t.Errorf("insert returned %d, want 2", id2)
	}

	// Test MatchQuery
	match, err := table.MatchQuery(1, "hello")
	if err != nil {
		t.Fatalf("MatchQuery error: %v", err)
	}
	if !match {
		t.Errorf("doc 1 should match 'hello'")
	}

	match, err = table.MatchQuery(2, "hello")
	if err != nil {
		t.Fatalf("MatchQuery error: %v", err)
	}
	if match {
		t.Errorf("doc 2 should NOT match 'hello'")
	}

	match, err = table.MatchQuery(1, "world")
	if err != nil {
		t.Fatalf("MatchQuery error: %v", err)
	}
	if !match {
		t.Errorf("doc 1 should match 'world'")
	}

	match, err = table.MatchQuery(2, "world")
	if err != nil {
		t.Fatalf("MatchQuery error: %v", err)
	}
	if !match {
		t.Errorf("doc 2 should match 'world'")
	}
}

func TestFTS3TableMatchDocIDs(t *testing.T) {
	table, _ := NewFTS3Table("test", "fts3", []string{"content"})
	table.Insert([]interface{}{"hello world"})
	table.Insert([]interface{}{"goodbye world"})
	table.Insert([]interface{}{"hello there"})

	// Match "hello" should return docs 1 and 3
	ids, err := table.MatchDocIDs("hello")
	if err != nil {
		t.Fatalf("MatchDocIDs error: %v", err)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
		t.Errorf("MatchDocIDs('hello') = %v, want [1 3]", ids)
	}

	// Match "hello OR goodbye" should return docs 1, 2, 3
	ids, err = table.MatchDocIDs("hello OR goodbye")
	if err != nil {
		t.Fatalf("MatchDocIDs error: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("MatchDocIDs('hello OR goodbye') = %v, want [1 2 3]", ids)
	}
}

func TestFTS3TableDelete(t *testing.T) {
	table, _ := NewFTS3Table("test", "fts3", []string{"content"})
	table.Insert([]interface{}{"hello world"})
	table.Insert([]interface{}{"goodbye world"})

	table.Delete(1)

	// Only doc 2 should remain
	ids, err := table.MatchDocIDs("world")
	if err != nil {
		t.Fatalf("MatchDocIDs error: %v", err)
	}
	if len(ids) != 1 || ids[0] != 2 {
		t.Errorf("after delete, MatchDocIDs('world') = %v, want [2]", ids)
	}
}

func TestFTS3TableUpdate(t *testing.T) {
	table, _ := NewFTS3Table("test", "fts3", []string{"content"})
	table.Insert([]interface{}{"hello world"})

	table.Update(1, []interface{}{"foo bar"})

	match, _ := table.MatchQuery(1, "hello")
	if match {
		t.Errorf("after update, doc should NOT match 'hello'")
	}

	match, _ = table.MatchQuery(1, "foo")
	if !match {
		t.Errorf("after update, doc should match 'foo'")
	}
}

func TestFTS3TableAllRows(t *testing.T) {
	table, _ := NewFTS3Table("test", "fts3", []string{"content"})
	table.Insert([]interface{}{"hello"})
	table.Insert([]interface{}{"world"})

	rows := table.AllRows()
	if len(rows) != 2 {
		t.Fatalf("AllRows() returned %d rows, want 2", len(rows))
	}
}

func TestNewTokenizer(t *testing.T) {
	if _, ok := NewTokenizer("simple").(*SimpleTokenizer); !ok {
		t.Errorf("NewTokenizer('simple') should return SimpleTokenizer")
	}
	if _, ok := NewTokenizer("unicode61").(*Unicode61Tokenizer); !ok {
		t.Errorf("NewTokenizer('unicode61') should return Unicode61Tokenizer")
	}
	if _, ok := NewTokenizer("porter").(*PorterTokenizer); !ok {
		t.Errorf("NewTokenizer('porter') should return PorterTokenizer")
	}
	if _, ok := NewTokenizer("unknown").(*SimpleTokenizer); !ok {
		t.Errorf("NewTokenizer('unknown') should return SimpleTokenizer (default)")
	}
}

func TestResolveColumnRef(t *testing.T) {
	colIndex := map[string]int{"title": 0, "body": 1}

	// Resolve a ColumnRefNode
	ref := &ColumnRefNode{ColumnName: "title", Inner: &TermNode{Term: "hello"}}
	resolved := ResolveColumnRef(ref, colIndex)
	colNode, ok := resolved.(*ColumnNode)
	if !ok {
		t.Fatalf("expected ColumnNode, got %T", resolved)
	}
	if colNode.Column != 0 {
		t.Errorf("column index = %d, want 0", colNode.Column)
	}
}

func TestCollectTerms(t *testing.T) {
	node := &AndNode{
		Left:  &TermNode{Term: "hello"},
		Right: &OrNode{
			Left:  &TermNode{Term: "world"},
			Right: &TermNode{Term: "foo"},
		},
	}
	terms := CollectTerms(node)
	if len(terms) != 3 {
		t.Errorf("CollectTerms returned %v, want 3 terms", terms)
	}
}

// --- FTS3Module tests ---

func TestFTS3ModuleLifecycle(t *testing.T) {
	mod := NewFTS3Module("fts3")
	if mod.ModuleName != "fts3" {
		t.Errorf("module name = %q, want %q", mod.ModuleName, "fts3")
	}

	// GetOrCreateTable
	table, err := mod.GetOrCreateTable("t1", "fts3", []string{"content"})
	if err != nil {
		t.Fatalf("GetOrCreateTable error: %v", err)
	}
	if table == nil {
		t.Fatal("table is nil")
	}

	// GetTable should find existing
	table2, ok := mod.GetTable("t1")
	if !ok || table2 != table {
		t.Errorf("GetTable returned different table")
	}

	// DropTable
	mod.DropTable("t1")
	_, ok = mod.GetTable("t1")
	if ok {
		t.Errorf("table should not exist after DropTable")
	}
}
