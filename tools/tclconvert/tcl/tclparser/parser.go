// SPDX-License-Identifier: GPL-3.0-or-later
package tclparser

// ParseCommands splits TCL source text into commands using the go-lemon
// generated LALR(1) parser. Each command is a slice of RawWord.
// This matches the output of the hand-written parser in the parent tcl package.
func ParseCommands(src string) [][]RawWord {
	lex := &tclLexer{src: src, atStart: true}
	tables := GetParseTables()
	parser := NewParser(tables)

	tclResult := &TclResult{}
	parser.ExtraCtx = tclResult
	parser.OnReduce(yyReduceAction)

	// Feed all tokens. After EOF, continue until the parser accepts.
	for {
		tokenType, value := lex.next()
		if tokenType == 0 {
			// Keep reducing with EOF until the parser accepts or errors.
			for !parser.IsDone() {
				status := parser.Parse(0, nil)
				if status == ParseError || status == ParseStackOverflow {
					break
				}
			}
			break
		}
		status := parser.Parse(tokenType, value)
		if status == ParseError || status == ParseStackOverflow {
			break
		}
		// ParseAccept after shift = keep feeding
	}

	parser.Finalize()

	if tclResult.Commands == nil {
		return [][]RawWord{}
	}
	return tclResult.Commands
}
