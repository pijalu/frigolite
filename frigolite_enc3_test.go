package frigolite

import (
	"os"
	"strings"
	"testing"
)

// TestNativeEncodingMismatchContract covers the engine-visible contract
// test/enc3.test 3.2 asserts (UTF-16le main DB rejects ATTACH of a UTF-8
// attached DB). Frigolite does not implement UTF-16 storage
// (SQLITE_OMIT_UTF16 is the build-option equivalent; src/sqliteInt.h
// SQLITE_OMIT_UTF16), so the native test below uses the UTF-8 setter /
// ATTACH-of-different-encoding path to validate the engine-side check.
//
// The testgen/enc3 test relies on test.db being UTF-16le (test 1.1,
// gated behind `ifcapable utf16` which the transpiler drops because
// Frigolite does not implement the UTF-16 capability). As a result
// test.db stays UTF-8 in the testgen flow and enc3-3.2 cannot observe
// the encoding mismatch. The contract is documented here against the
// native API.
func TestNativeEncodingMismatchContract(t *testing.T) {
	dir := t.TempDir()
	mainPath := dir + "/main.db"
	auxPath := dir + "/aux.db"

	main, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	if r := main.Exec("CREATE TABLE t1(x)"); r.Error != nil {
		t.Fatalf("create main: %v", r.Error)
	}

	// Open the aux DB in a separate connection so it has its own pager
	// and header bytes that the attach path can read.
	aux, err := Open(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	aux.Close()

	// ATTACH should succeed because both DBs default to UTF-8 (matches
	// main's encoding).
	if r := main.Exec("ATTACH '" + auxPath + "' AS aux1"); r.Error != nil {
		t.Fatalf("attach same-encoding should succeed, got: %v", r.Error)
	}

	// Manually patch the aux DB header to claim UTF-16le (header byte 56-57
	// is the text encoding; src/sqliteInt.h: SqliteCookie TextEncoding).
	// After this, ATTACH (on a fresh connection) must fail with the
	// "attached databases must use the same text encoding as main
	// database" error message.
	main2, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer main2.Close()
	// Best-effort: corrupt the aux file's text encoding byte via direct
	// write so the encoding check fires. SQLite's encoding mismatch check
	// (src/attach.c:200) reads TextEncoding from the attached DB's header
	// and compares to main's; with both being 0 (default UTF-8) they
	// match. We simulate the post-UTF-16le-main case by writing the
	// UTF-16le magic to the aux file directly. This validates the
	// checkAttachEncoding contract without requiring UTF-16 storage
	// support.
	if err := setFileTextEncoding(auxPath, 2); err != nil { // 2 = UTF-16le
		t.Fatalf("setFileTextEncoding: %v", err)
	}
	r := main2.Exec("ATTACH '" + auxPath + "' AS aux2")
	if r.Error == nil {
		t.Fatalf("ATTACH different-encoding should fail, got nil")
	}
	if !strings.Contains(r.Error.Error(), "attached databases must use the same text encoding") {
		t.Fatalf("expected encoding-mismatch error, got: %v", r.Error)
	}
}

// setFileTextEncoding patches the on-disk text encoding byte of the SQLite
// header (file offset 56, big-endian uint32 per src/sqlite.h). Used only by
// the native encoding-mismatch test to simulate a UTF-16le file when the
// engine does not implement UTF-16 storage.
func setFileTextEncoding(path string, enc uint32) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte{
		byte(enc >> 24), byte(enc >> 16), byte(enc >> 8), byte(enc),
	}, 56); err != nil {
		return err
	}
	return nil
}