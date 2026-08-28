%token_prefix TK_
%token_type {interface{}}
%default_type {interface{}}
%name TclParser
%extra_argument {*TclResult result}

%token BRACE_WORD QUOTE_WORD BARE_WORD SEPARATOR.

_START ::= input.
input ::= commands.              { result.(*TclResult).Commands = $1.([][]RawWord) }

commands ::= commands SEPARATOR words.   { $$ = append($1.([][]RawWord), $3.([]RawWord)) }
commands ::= words.                      { $$ = [][]RawWord{$1.([]RawWord)} }
commands ::= commands SEPARATOR.         { $$ = $1 }
commands ::= .                           { $$ = [][]RawWord{} }

words ::= words word.                    { $$ = append($1.([]RawWord), $2.(RawWord)) }
words ::= word.                          { $$ = []RawWord{$1.(RawWord)} }

word ::= BRACE_WORD.                     { $$ = $1 }
word ::= QUOTE_WORD.                     { $$ = $1 }
word ::= BARE_WORD.                      { $$ = $1 }
