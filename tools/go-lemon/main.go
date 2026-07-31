// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// go-lemon: Pure Go LALR(1) parser generator.
//
// Usage: go-lemon [options] grammar.y
//
// Options:
//   -o file    Output Go file (default: grammar.go)
//   -p pkg     Package name (default: main)
//   -v         Verbose (print grammar stats)
//
// go-lemon reads a Lemon grammar file (.y format, compatible with
// SQLite's parse.y) and generates a Go source file containing an
// LALR(1) parser for that grammar.

package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	outputFlag := flag.String("o", "", "Output Go file (default: <grammar>.go)")
	pkgFlag := flag.String("p", "main", "Package name")
	verboseFlag := flag.Bool("v", false, "Verbose output")
	convertFlag := flag.Bool("convert", false, "Convert C lemon parse.c to Go tables")
	flag.Parse()

	if *convertFlag {
		// Conversion mode: go-lemon -convert parse.c [output.go]
		if flag.NArg() < 1 {
			fmt.Fprintf(os.Stderr, "Usage: go-lemon -convert parse.c [output.go]\n")
			os.Exit(1)
		}
		inputFile := flag.Arg(0)
		outputFile := *outputFlag
		if outputFile == "" {
			if strings.HasSuffix(inputFile, ".c") {
				outputFile = inputFile[:len(inputFile)-2] + "_tables.go"
			} else {
				outputFile = inputFile + "_tables.go"
			}
		}
		if err := ConvertTables(inputFile, outputFile); err != nil {
			fmt.Fprintf(os.Stderr, "Error converting tables: %v\n", err)
			os.Exit(1)
		}
		if *verboseFlag {
			fmt.Printf("Converted %s -> %s\n", inputFile, outputFile)
		}
		return
	}

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: go-lemon [options] grammar.y\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	grammarFile := flag.Arg(0)

	// Parse the grammar
	grammar, err := ParseGrammar(grammarFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing grammar: %v\n", err)
		os.Exit(1)
	}

	if *verboseFlag {
		grammar.PrintGrammarStats()
	}

	// Determine output file
	outputFile := *outputFlag
	if outputFile == "" {
		if strings.HasSuffix(grammarFile, ".y") {
			outputFile = grammarFile[:len(grammarFile)-2] + ".go"
		} else {
			outputFile = grammarFile + ".go"
		}
	}

	// Generate LALR(1) parse tables
	tables, err := GenerateTables(grammar)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating tables: %v\n", err)
		os.Exit(1)
	}

	// Generate Go output with tables embedded.
	// Token codes MUST match buildParseTables' symIndex (terminal-only
	// 1-based indexing), otherwise the generated constants and the tables
	// disagree and every engine lookup misses.
	tokenCode := make(map[string]int)
	tokCode := 1
	for _, sym := range grammar.Symbols {
		if sym.Type == TermSymbol {
			tokenCode[sym.Name] = tokCode
			tokCode++
		}
	}
	outCode := GenerateGoOutputFromTables(tables, grammar, tokenCode, *pkgFlag)

	if err := os.WriteFile(outputFile, []byte(outCode), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output: %v\n", err)
		os.Exit(1)
	}

	if *verboseFlag {
		fmt.Printf("Generated %s (%d bytes)\n", outputFile, len(outCode))
	}
}
