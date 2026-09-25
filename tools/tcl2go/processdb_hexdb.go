// Package main implements the tcl2go tool.
//
// This file decodes the sqlite3 .open --hexdb image format (extractHexdbBlock,
// parseHexdbImage) and emits the `db deserialize VALUE` forms that carry a Go
// image variable rather than a hexdb block.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// extractHexdbBlock pulls the braced block out of `[decode_hexdb {...}]`.
func extractHexdbBlock(text string) string {
	idx := strings.Index(text, "{")
	if idx < 0 {
		return ""
	}
	depth := 0
	for i := idx; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[idx+1 : i]
			}
		}
	}
	return text[idx+1:]
}

// parseHexdbImage converts an .open --hexdb block into the raw database bytes.
// Lines are `| <offset>: <hex bytes>  <ascii>` grouped by `| page N offset M`.
func parseHexdbImage(hexdb string) ([]byte, error) {
	// The header lines carry the total size; pages fill the rest.
	size, pageSize := hexdbHeaderSize(hexdb)
	if size <= 0 {
		size = pageSize // fall back to one page
	}
	out := make([]byte, size)
	hexdbFillPages(out, hexdb, pageSize)
	return out, nil
}

// hexdbHeaderSize scans the `| size N pagesize M` header lines and returns
// the declared image size and page size (page size defaults to 4096).
func hexdbHeaderSize(hexdb string) (size, pageSize int) {
	size, pageSize = 0, 4096
	for _, line := range strings.Split(hexdb, "\n") {
		line = strings.TrimSpace(line)
		if m := hexdbKV(line, "size"); m != "" {
			size, _ = strconv.Atoi(m)
		}
		if m := hexdbKV(line, "pagesize"); m != "" {
			pageSize, _ = strconv.Atoi(m)
		}
	}
	return size, pageSize
}

// hexdbFillPages copies every `<offset>: <hex bytes>` dump line into out at
// its page-relative position.
func hexdbFillPages(out []byte, hexdb string, pageSize int) {
	curPage := 0
	for _, line := range strings.Split(hexdb, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		rest := strings.TrimSpace(trimmed[1:])
		if strings.HasPrefix(rest, "page ") {
			// page N offset M
			curPage = hexdbPageNumber(rest, curPage)
			continue
		}
		// <offset>: <hex bytes>
		hexdbFillLine(out, rest, curPage, pageSize)
	}
}

// hexdbPageNumber parses a `page N offset M` line, returning the current page
// number (unchanged when N does not parse).
func hexdbPageNumber(rest string, cur int) int {
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return cur
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil {
		return cur
	}
	return n
}

// hexdbFillLine decodes one `<offset>: <hex bytes>` line into out. Hex bytes
// are the first 2-char groups; the ASCII column is stripped.
func hexdbFillLine(out []byte, rest string, curPage, pageSize int) {
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return
	}
	off, _ := strconv.Atoi(strings.TrimSpace(rest[:colon]))
	pairs := hexBytePairs(rest[colon+1:])
	for i, b := range pairs {
		pos := (curPage - 1)*pageSize + off + i
		if pos < len(out) {
			out[pos] = b
		}
	}
}

// hexdbKV extracts a `key value` pair from an .open header line like
// `| size 24576 pagesize 4096 filename x`.
func hexdbKV(line, key string) string {
	for i := 0; i+len(key) <= len(line); i++ {
		if line[i:i+len(key)] == key {
			j := i + len(key)
			for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
				j++
			}
			k := j
			for k < len(line) && line[k] != ' ' && line[k] != '\t' {
				k++
			}
			return line[j:k]
		}
	}
	return ""
}

// hexBytePairs extracts 2-hex-digit byte values from a hex dump line (the
// part before the ASCII column).
func hexBytePairs(s string) []byte {
	var out []byte
	for i := 0; i+1 < len(s); i++ {
		if isHexByte(s[i]) && isHexByte(s[i+1]) {
			if i+2 < len(s) && isHexByte(s[i+2]) {
				// A triple of hex digits is a stray run (ASCII column); stop.
				break
			}
			b := byte((hexByteVal(s[i]) << 4) | hexByteVal(s[i+1]))
			out = append(out, b)
			i++
		}
	}
	return out
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexByteVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// deserializeOptions holds the parsed `db deserialize` flag values as Go
// expressions (the "0"/"false" defaults stand for unlimited read-write).
type deserializeOptions struct {
	maxsize  string
	readonly string
}

// emitDBDeserializeValue handles `db deserialize [--flags] [SCHEMA] VALUE`
// (memdb1.test: $db1/db1Blob image, aux-schema images, {} empty reset,
// not-a-database corruption, -readonly/-maxsize flags, unknown options).
// VALUE renders as a Go string expression; the bytes deserialize into the
// named schema via DB.Deserialize (tclsqlite.c DB_DESERIALIZE contract).
func (tp *transpiler) emitDBDeserializeValue(rest []tcl.RawWord) {
	opts, args, ok := tp.deserializeFlags(rest)
	if !ok {
		return
	}
	if len(args) == 0 {
		tp.emitDeserializeWrongArgs()
		return
	}
	schema, value := "main", ""
	switch {
	case len(args) == 2:
		schema = strings.Trim(args[0].Text, "{} ")
		value = tp.deserializeValueExpr(args[1])
	case len(args) == 1:
		value = tp.deserializeValueExpr(args[0])
	default:
		tp.emitDeserializeUnknownOption(strings.TrimSpace(args[0].Text))
		return
	}
	tp.emitLine("if derr := db.Deserialize(%q, []byte(%s), frigolite.DeserializeOptions{ReadOnly: %s, MaxSize: %s}); derr != nil { tclDeserializeErr = derr } else { tclDeserializeErr = nil }", schema, value, opts.readonly, opts.maxsize)
	// Inside a db-eval row callback (BeginActiveStatement open) or a
	// catch-mode body, the deserialize error feeds the harness error
	// variable (_catchErr), not a hard t.Errorf — memdb1.test 1010 expects
	// {1 {unable to set MEMDB content}} from the catch, and 1020's backup
	// interlock likewise flows through msg.
	if tp.catchMode || tp.inDBEvalCb {
		tp.emitLine("if tclDeserializeErr != nil { _catchErr = tclDeserializeErr }")
	} else {
		tp.emitLine("if tclDeserializeErr != nil { t.Errorf(%q, tclDeserializeErr) }", "deserialize failed: %v")
	}
}

// deserializeFlags parses the leading -maxsize/-readonly flags (and rejects
// unknown options) off a `db deserialize` argument list. It returns the flag
// values and the remaining positional arguments; ok is false when an unknown
// option already completed the emission.
func (tp *transpiler) deserializeFlags(rest []tcl.RawWord) (deserializeOptions, []tcl.RawWord, bool) {
	opts := deserializeOptions{maxsize: "0", readonly: "false"}
	i := 0
	for i < len(rest) {
		w := strings.TrimSpace(rest[i].Text)
		if w == "-maxsize" && i+1 < len(rest) {
			maxsize := tp.buildStringExpr(strings.TrimSpace(rest[i+1].Text))
			opts.maxsize = fmt.Sprintf("tclParseInt64(%s)", maxsize)
			i += 2
			continue
		}
		if w == "-readonly" && i+1 < len(rest) {
			bexpr := tp.buildStringExpr(strings.TrimSpace(rest[i+1].Text))
			opts.readonly = fmt.Sprintf("tclBool(%s)", bexpr)
			i += 2
			continue
		}
		if strings.HasPrefix(w, "-") {
			// TCL `catch {db deserialize -unknown 1 $db1} msg` consumes
			// the unknown flag AND its value (memdb1.test 150): the error
			// is "unknown option: -unknown", and no deserialize runs.
			// In catch mode the error must reach _catchErr; in direct
			// mode the do_test body comparison runs against _r.
			tp.emitDeserializeUnknownOption(strings.Trim(w, "{} "))
			return opts, rest, false
		}
		break
	}
	return opts, rest[i:], true
}

// emitDeserializeUnknownOption reports an unrecognized `db deserialize`
// option: catch mode raises it as _catchErr, direct mode stores it in _r.
func (tp *transpiler) emitDeserializeUnknownOption(opt string) {
	if tp.catchMode {
		tp.emitLine("_catchErr = fmt.Errorf(%q)", "unknown option: "+opt)
	} else {
		tp.emitLine("_r = %q", "unknown option: "+opt)
	}
}

// emitDeserializeWrongArgs reports a `db deserialize` invocation with no
// VALUE word.
func (tp *transpiler) emitDeserializeWrongArgs() {
	if tp.catchMode {
		tp.emitLine("_catchErr = fmt.Errorf(%q)", "wrong # args: should be \"db deserialize ?DATABASE? VALUE\"")
	} else {
		tp.emitLine("t.Errorf(%q)", "wrong # args: should be \"db deserialize ?DATABASE? VALUE\"")
	}
}

// deserializeValueExpr renders a deserialize VALUE word as a Go string:
// $::db1 maps to the db1Blob image shadow; $vars map to Go vars; braced
// literals ({} empty, not-a-database) render verbatim.
func (tp *transpiler) deserializeValueExpr(w tcl.RawWord) string {
	text := strings.TrimSpace(w.Text)
	if text == "$::db1" || text == "$db1" || text == "::db1" || text == "db1" {
		return "db1Blob"
	}
	if strings.HasPrefix(text, "$") {
		return tp.buildStringExpr(text)
	}
	return tp.buildStringExpr(text)
}
