package vtab

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/pijalu/frigolite/internal/value"
)

// ZipfileModule implements the zipfile virtual table (ext/misc/zipfile.c):
// read/write access to ZIP archives, either through a file path or an
// in-memory archive blob passed as a table-function argument.
//
// Schema (ZIPFILE_SCHEMA):
//
//	CREATE TABLE y(name PRIMARY KEY, mode, mtime, sz, rawdata,
//	               data, method, z HIDDEN) WITHOUT ROWID
type ZipfileModule struct{}

// ConnectWithValues preserves binary archive arguments supplied as SQL BLOBs
// (zipfile.c xFilter: SQLITE_BLOB binds an in-memory archive). Arity and NULL
// handling mirror zipfile.c: zero arguments report the table-valued-function
// message, a NULL argument is an empty file name ("cannot open file:").
func (m *ZipfileModule) ConnectWithValues(args []interface{}) (VirtualTable, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("zipfile() function requires an argument")
	}
	a := zipValueArgString(args[0])
	return m.connect([]string{a}, false)
}

// CreateWithValues preserves binary archive arguments for CREATE VIRTUAL
// TABLE form instances; arity errors mirror zipfile.c's constructor message.
func (m *ZipfileModule) CreateWithValues(args []interface{}) (VirtualTable, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("zipfile constructor requires one argument")
	}
	a := zipValueArgString(args[0])
	return m.connect([]string{a}, true)
}

// zipValueArgString renders one typed vtab argument as its TEXT argv value;
// a NULL argument becomes the empty string (SQLite passes a NULL pointer,
// which fopen then fails on).
func zipValueArgString(arg interface{}) string {
	switch v := arg.(type) {
	case []byte:
		return string(v)
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// NewZipfileModule builds the zipfile module.
func NewZipfileModule() *ZipfileModule { return &ZipfileModule{} }

// Eponymous implements EponymousModule (zipfile supports both
// CREATE VIRTUAL TABLE and direct FROM use).
func (m *ZipfileModule) Eponymous() bool { return true }

// Columns returns the declared schema column names.
func (m *ZipfileModule) Columns() []string {
	return []string{"name", "mode", "mtime", "sz", "rawdata", "data", "method", "z"}
}

// zipBlobStore shares in-memory archives across instances: every DML or
// SELECT statement creates a fresh instance from the module argv, so writes
// through one instance must be visible to later instances bound to the same
// blob argument.
var (
	zipBlobMu    sync.Mutex
	zipBlobStore = map[string]string{}
)

// zipBlobLoad returns the current archive bytes for a blob-backed source.
func zipBlobLoad(key string) (string, bool) {
	zipBlobMu.Lock()
	defer zipBlobMu.Unlock()
	v, ok := zipBlobStore[key]
	return v, ok
}

// zipBlobStoreSave persists archive bytes for a blob-backed source.
func zipBlobSave(key, val string) {
	zipBlobMu.Lock()
	defer zipBlobMu.Unlock()
	zipBlobStore[key] = val
}

// zipEntry is one archive member held in memory.
type zipEntry struct {
	name       string
	mode       uint32 // external attrs >> 16
	munix      int64  // unix mtime
	method     uint16 // 0 stored, 8 deflate
	data       []byte // uncompressed content (nil when method is unknown)
	raw        []byte // stored payload as-is (rawdata column)
	crc        uint32
	dosTime    uint16
	dosDate    uint16
	hasUTStamp bool
}

// zipfileVTab is one bound instance (archive source fixed at create time).
type zipfileVTab struct {
	filePath string // empty when dataArg holds the archive
	dataArg  string // in-memory archive bytes (Go string carries bytes)
	columns  []string
}

// Create implements Module (CREATE VIRTUAL TABLE form).
func (m *ZipfileModule) Create(args []string) (VirtualTable, error) {
	if len(args) == 0 {
		// DML against a bare eponymous name resolves through xCreate with
		// no argv: zipfile.c reports the missing-filename error
		// (DELETE FROM zipfile).
		return nil, fmt.Errorf("zipfile: missing filename")
	}
	if len(args) != 1 || strings.TrimSpace(unquoteVtabArg(strings.TrimSpace(args[0]))) == "" {
		return nil, fmt.Errorf("zipfile constructor requires one argument")
	}
	return m.connect(args, true)
}

// Connect implements Module (table-valued function form).
func (m *ZipfileModule) Connect(args []string) (VirtualTable, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("zipfile() function requires an argument")
	}
	return m.connect(args, false)
}

func (m *ZipfileModule) connect(args []string, createOK bool) (VirtualTable, error) {
	v := &zipfileVTab{columns: m.Columns()}
	if len(args) == 0 {
		// zipfile.c zipfileCreate: no argument at all.
		return nil, fmt.Errorf("zipfile: missing filename")
	}
	if len(args) != 1 {
		return nil, fmt.Errorf("zipfile constructor requires one argument")
	}
	if strings.TrimSpace(args[0]) == "" {
		// NULL/empty archive source (SELECT * FROM zipfile(NULL)).
		return nil, fmt.Errorf("error in zipfile module: cannot open file: %s", args[0])
	}
	if len(args) > 0 {
		// The argument may be "name = value" form (CREATE VIRTUAL TABLE)
		// or a bare value (table-function call).
		a := strings.TrimSpace(args[0])
		if eq := strings.Index(a, "="); eq >= 0 && !strings.ContainsAny(a[:eq], "/.") {
			a = strings.TrimSpace(a[eq+1:])
		}
		a = unquoteVtabArg(a)
		if looksLikeFilePath(a) {
			if st, serr := os.Stat(a); serr == nil && st.IsDir() {
				// SQLite's zipfile opens the archive lazily; a directory
				// connects fine (empty central directory) and only the
				// first write fails (zipfile.test 8.1.x).
				v.filePath = a
				return v, nil
			}
			if _, serr := os.Stat(a); serr != nil {
				if !createOK {
					// The table-valued form reads an EXISTING archive;
					// only the CREATE VIRTUAL TABLE form creates one
					// (zipfile.test 19.x).
					return nil, fmt.Errorf("error in zipfile module: cannot open file: %s", a)
				}
				f, ferr := os.OpenFile(a, os.O_CREATE|os.O_RDWR, 0644)
				if ferr != nil {
					return nil, fmt.Errorf("error in zipfile module: cannot open file: %s", a)
				}
				f.Close()
			}
			v.filePath = a
		} else {
			v.dataArg = a
		}
	}
	return v, nil
}

// unquoteVtabArg strips one level of matching quotes from an argv value.
// SQLite hands vtab modules VERBATIM argument spans (tokenize.c CC_QUOTE /
// CC_QUOTE2 keep the quote characters in the token; parse.y captures them
// via %wildcard ANY + sqlite3VtabArgExtend), so every module dequotes its
// own arguments. This mirrors ext/misc/unionvtab.c unionDequote: all four
// SQL quote openers ([ ' " ` with ] closing bracket-quoted names) are
// stripped, and a doubled quote inside collapses to a single literal quote
// (unionvtab: 'SELECT ... db!=”xyz”').
func unquoteVtabArg(a string) string {
	if len(a) < 2 {
		return a
	}
	var closer byte
	switch a[0] {
	case '\'':
		closer = '\''
	case '"':
		closer = '"'
	case '`':
		closer = '`'
	case '[':
		closer = ']'
	default:
		return a
	}
	if a[len(a)-1] != closer {
		// Not a well-formed quoted span (e.g. edit_cost_table=x'); leave
		// the argument untouched, as unionDequote would.
		return a
	}
	var b strings.Builder
	b.Grow(len(a) - 2)
	for i := 1; i < len(a)-1; i++ {
		if a[i] == closer && i+1 < len(a)-1 && a[i+1] == closer {
			b.WriteByte(closer)
			i++
			continue
		}
		b.WriteByte(a[i])
	}
	return b.String()
}

// looksLikeFilePath reports whether a module argument names a FILE rather
// than carrying archive bytes: printable text without NULs. A real zip blob
// starts with binary bytes (PK\x03\x04), so NUL/control bytes or invalid
// UTF-8 mark blob content. Missing files still count as paths — SQLite
// creates them on write.
func looksLikeFilePath(a string) bool {
	if a == "" || len(a) > 512 {
		return false
	}
	if !utf8.ValidString(a) {
		return false
	}
	if strings.ContainsAny(a, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0c\x0e\x0f") {
		return false
	}
	for i := 0; i < len(a); i++ {
		if a[i] < 0x20 && a[i] != '\n' && a[i] != '\r' && a[i] != '\t' {
			return false
		}
	}
	return true
}

// HiddenColumns implements HiddenColumnInfo: the z column is hidden.
func (v *zipfileVTab) HiddenColumns() map[int]bool { return map[int]bool{7: true} }

// Columns implements ColumnInfo.
func (v *zipfileVTab) Columns() []string { return v.columns }

// PrimaryKeyColumns implements PrimaryKeyInfo: the declared schema marks
// name as PRIMARY KEY (ZIPFILE_SCHEMA).
func (v *zipfileVTab) PrimaryKeyColumns() map[int]bool { return map[int]bool{0: true} }

// BestIndex accepts the default full-scan plan.
func (v *zipfileVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// loadEntries parses the bound archive into memory.
func (v *zipfileVTab) loadEntries() ([]zipEntry, error) {
	var raw []byte
	if v.filePath != "" {
		b, err := os.ReadFile(v.filePath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil // SQLite treats an unwritten archive as empty
			}
			// A directory path connects as an empty archive; the write
			// itself fails later with the fopen("ab+") message.
			return nil, nil
		}
		raw = b
	} else {
		if shared, ok := zipBlobLoad(v.dataArg); ok {
			raw = []byte(shared)
		} else {
			raw = []byte(v.dataArg)
		}
	}
	return zipParseEntries(raw)
}

// Open implements VirtualTable: scan all entries in central-directory order.
func (v *zipfileVTab) Open() (Cursor, error) {
	entries, err := v.loadEntries()
	if err != nil {
		return nil, err
	}
	src := v.archiveSource()
	return &zipCursor{entries: entries, src: src}, nil
}

func (v *zipfileVTab) archiveSource() string {
	if v.filePath != "" {
		return v.filePath
	}
	return v.dataArg
}

// zipParseEntries extracts entries from a ZIP archive's central directory.
// It understands stored and deflate members plus the extended-timestamp
// extra field (0x5455) used by zipfile.c.
func zipParseEntries(raw []byte) ([]zipEntry, error) {
	eocd := bytes.LastIndex(raw, []byte{0x50, 0x4b, 0x05, 0x06})
	if eocd < 0 {
		if len(raw) == 0 {
			return nil, nil
		}
		// zipfileReadEOCD reports this defect verbatim; the engine adds no
		// "error in zipfile module:" prefix to it (zipfile2 4.3.*).
		return nil, fmt.Errorf("cannot find end of central directory record")
	}
	n := int(binary.LittleEndian.Uint16(raw[eocd+10 : eocd+12]))
	off := int(binary.LittleEndian.Uint32(raw[eocd+16 : eocd+20]))
	// zipfile.c zipfileLoadDirectory: a central directory that claims
	// entries outside the image is corruption (zipfile.test 17.x).
	if n > 0 && (off < 0 || off+46 > len(raw) || binary.LittleEndian.Uint32(raw[off:off+4]) != 0x02014b50) {
		return nil, fmt.Errorf("error in zipfile module: zip archive is corrupt")
	}
	var out []zipEntry
	for i := 0; i < n; i++ {
		// zipfile.c requires EVERY declared central-directory record to be
		// present with a valid signature; a truncated or mis-signed record is
		// corruption, never a silent truncation (zipfile2 3.3 patched PK
		// signatures must error).
		if off+46 > len(raw) || binary.LittleEndian.Uint32(raw[off:off+4]) != 0x02014b50 {
			return nil, fmt.Errorf("error in zipfile module: zip archive is corrupt")
		}
		e := zipEntry{
			method:  binary.LittleEndian.Uint16(raw[off+10 : off+12]),
			dosTime: binary.LittleEndian.Uint16(raw[off+12 : off+14]),
			dosDate: binary.LittleEndian.Uint16(raw[off+14 : off+16]),
			crc:     binary.LittleEndian.Uint32(raw[off+16 : off+20]),
			mode:    uint32(binary.LittleEndian.Uint32(raw[off+38:off+42])) >> 16,
		}
		szComp := int(binary.LittleEndian.Uint32(raw[off+20 : off+24]))
		nName := int(binary.LittleEndian.Uint16(raw[off+28 : off+30]))
		nExtra := int(binary.LittleEndian.Uint16(raw[off+30 : off+32]))
		lho := int(binary.LittleEndian.Uint32(raw[off+42 : off+46]))
		if off+46+nName+nExtra > len(raw) {
			return nil, fmt.Errorf("error in zipfile module: zip archive is corrupt")
		}
		e.name = string(raw[off+46 : off+46+nName])
		// Extended timestamp (0x5455) in the central directory carries the
		// unix mtime; fall back to decoding the DOS fields.
		extra := raw[off+46+nName : off+46+nName+nExtra]
		e.munix = dosToUnix(e.dosDate, e.dosTime)
		for j := 0; j+5 <= len(extra); {
			id := binary.LittleEndian.Uint16(extra[j : j+2])
			sz := int(binary.LittleEndian.Uint16(extra[j+2 : j+4]))
			if id == 0x5455 && sz >= 5 {
				e.munix = int64(binary.LittleEndian.Uint32(extra[j+5 : j+9]))
				e.hasUTStamp = true
				break
			}
			j += 4 + sz
		}
		// Payload lives after the local header's name+extra.
		// Local headers may carry the only extended timestamp (SQLite accepts
		// it when the central-directory extra field omits UT).
		if !e.hasUTStamp && lho+30 <= len(raw) {
			ln := int(binary.LittleEndian.Uint16(raw[lho+26 : lho+28]))
			lx := int(binary.LittleEndian.Uint16(raw[lho+28 : lho+30]))
			if lho+30+ln+lx <= len(raw) {
				if ts, ok := zipUTTimestamp(raw[lho+30+ln : lho+30+ln+lx]); ok {
					e.munix, e.hasUTStamp = ts, true
				}
			}
		}
		// Payload lives after the local header's name+extra.
		// zipfile.c reads the LFH with a signed 32-bit offset; an offset
		// that is negative or past the image is a read failure
		// ("failed to read LFH at offset %lld", zipfile.test 17.x).
		signedLho := int64(int32(uint32(lho)))
		if signedLho < 0 || lho+30 > len(raw) {
			return nil, fmt.Errorf("error in zipfile module: failed to read LFH at offset %d", signedLho)
		}
		// zipfileReadLFH verifies the local-header magic and reports the
		// read failure with the record's offset (zipfile2 3.3/8.x patched
		// signatures must fail with this message).
		if binary.LittleEndian.Uint32(raw[lho:lho+4]) != 0x04034b50 {
			return nil, fmt.Errorf("error in zipfile module: failed to read LFH at offset %d", signedLho)
		}
		nLN := int(binary.LittleEndian.Uint16(raw[lho+26 : lho+28]))
		nLX := int(binary.LittleEndian.Uint16(raw[lho+28 : lho+30]))
		start := lho + 30 + nLN + nLX
		end := start + szComp
		if end <= len(raw) {
			payload := raw[start:end]
			e.raw = payload
			switch e.method {
			case 0, 8:
				// zipfile.c decompresses during cursor reads and surfaces zlib
				// failures as statement errors (zipfile2 4.1: a patched deflate
				// stream must fail the SELECT with "inflate() failed").
				data, err := zipInflate(e.method, payload, e.crc, int(binary.LittleEndian.Uint32(raw[off+24:off+28])))
				if err != nil {
					return nil, err
				}
				e.data = data
			default:
				// Unknown methods keep only the stored payload: sqlite's data
				// column returns NULL for them (zipfileColumn's method guard),
				// while rawdata still exposes e.raw (zipfile2 4.2).
				e.data = nil
			}
		}
		out = append(out, e)
		off += 46 + nName + nExtra
	}
	return out, nil
}

// zipInflate decompresses payload for method 0 (stored) or 8 (deflate),
// verifying the CRC-32 when known.
func zipInflate(method uint16, payload []byte, crc uint32, szUncompressed int) ([]byte, error) {
	inflateFailed := fmt.Errorf("error in zipfile module: inflate() failed (0)")
	switch method {
	case 0:
		return payload, nil
	case 8:
		r := flate.NewReader(bytes.NewReader(payload))
		defer r.Close()
		out, err := io.ReadAll(r)
		if err != nil {
			// SQLite surfaces zlib failures verbatim (zipfile2 3.x patches
			// archive bytes to force exactly this).
			return nil, inflateFailed
		}
		// zlib rejects streams whose decompressed image fails its integrity
		// framing; Go's flate reader is more lenient and can emit plausible
		// garbage for a patched stream (zipfile2 4.1 patches bytes inside
		// deflate payloads). A CRC-32 mismatch is the same defect, so report
		// the identical error.
		if crc != 0 && crc32.ChecksumIEEE(out) != crc {
			return nil, inflateFailed
		}
		// zlib's avail_out contract: the inflated image must consume exactly
		// szUncompressed bytes (sqlite passes pCDS->szUncompressed); a patch
		// that shifts sizes yields a different stream length → same error.
		if szUncompressed >= 0 && len(out) != szUncompressed {
			return nil, inflateFailed
		}
		return out, nil
	default:
		return nil, fmt.Errorf("zipfile: unknown compression method: %d", method)
	}
}

// zipDeflate compresses data for the given method.
func zipDeflate(method uint16, data []byte) []byte {
	if method == 0 {
		return data
	}
	var buf bytes.Buffer
	// Level 9 mirrors SQLite's zlib deflate shrinkage behaviour closely
	// enough for the method auto-selection tests (h.txt 20-byte payload
	// must compress below its raw size to select method 8).
	w, _ := flate.NewWriter(&buf, 9)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// dosToUnix ports zipfileMtime (ext/misc/zipfile.c) verbatim: the DOS
// date/time fields decode through Julian-day arithmetic. There is NO
// zero-value shortcut — an all-zero date decodes to 1979-11-30T00:00:00Z =
// 312768000 (zipfile.test 22.x crafted archive row), which a naive
// time.Date(1980,0,0) construction cannot produce.
func dosToUnix(dosDate, dosTime uint16) int64 {
	Y := int64(1980 + ((dosDate >> 9) & 0x7F))
	M := int64((dosDate >> 5) & 0x0F)
	D := int64(dosDate & 0x1F)
	sec := int64((dosTime & 0x1F) * 2)
	min := int64((dosTime >> 5) & 0x3F)
	hr := int64((dosTime >> 11) & 0x1F)
	if M <= 2 {
		Y--
		M += 12
	}
	X1 := int64(36525 * (Y + 4716) / 100)
	X2 := int64(306001 * (M + 1) / 10000)
	A := Y / 100
	B := int64(2 - A + A/4)
	// X1+X2+D+B-1524.5 is exactly N.5 for integer day arithmetic, so
	// (…-1524.5)*86400 truncates to whole days; the i64 cast happens after
	// the float multiply, exactly as the C code does.
	JDsec := int64((float64(X1+X2+D+B)-1524.5)*86400) + hr*3600 + min*60 + sec
	return JDsec - int64(24405875)*int64(8640)
}

// zipDosFromUnix mirrors zipfileMtimeToDos (Julian-day arithmetic so dates
// before 1970 behave identically; pre-1980 collapses to 0/0).
func zipDosFromUnix(munix int64) (dosDate, dosTime uint16) {
	JD := 2440588 + munix/(24*60*60)
	A := int((float64(JD) - 1867216.25) / 36524.25)
	A = int(JD + 1 + int64(A) - int64(A/4))
	B := A + 1524
	C := int((float64(B) - 122.1) / 365.25)
	D := (36525 * (C & 32767)) / 100
	E := int((float64(B) - float64(D)) / 30.6001)

	day := B - D - int(30.6001*float64(E))
	mon := E - 1
	if E >= 14 {
		mon = E - 13
	}
	yr := C - 4716
	if mon <= 2 {
		yr = C - 4715
	}
	hr := munix % (24 * 60 * 60) / (60 * 60)
	min := munix % (60 * 60) / 60
	sec := munix % 60

	if yr < 1980 {
		return 0, 0
	}
	return uint16(day + (mon << 5) + ((yr - 1980) << 9)),
		uint16(sec/2 + (min << 5) + (hr << 11))
}

// isDir reports whether the entry carries directory semantics.
func (e *zipEntry) isDir() bool {
	return e.mode&0040000 != 0 || (len(e.data) == 0 && strings.HasSuffix(e.name, "/"))
}

// zipCursor scans parsed entries.
type zipCursor struct {
	entries []zipEntry
	idx     int
	src     string
	started bool
}

// Next implements Cursor (first row already positioned).
func (c *zipCursor) Next() bool {
	if !c.started {
		c.started = true
		return len(c.entries) > 0
	}
	c.idx++
	return c.idx < len(c.entries)
}

// Column implements Cursor.
func (c *zipCursor) Column(idx int) (interface{}, error) {
	if c.idx < 0 || c.idx >= len(c.entries) {
		return nil, fmt.Errorf("no row")
	}
	e := c.entries[c.idx]
	switch idx {
	case 0:
		// zipfile.c stores the entry name via sqlite3_mprintf("%.*s"), so a
		// name embedding NUL truncates at the first NUL byte when returned
		// as TEXT (zipfile.test 22.x crafted archive: "A\0BBB…" reads as
		// "A").
		if i := strings.IndexByte(e.name, 0); i >= 0 {
			return e.name[:i], nil
		}
		return e.name, nil
	case 1:
		return int64(e.mode), nil
	case 2:
		return e.munix, nil
	case 3:
		return int64(len(e.data)), nil
	case 4:
		if e.isDir() {
			return nil, nil
		}
		// rawdata: the STORED payload verbatim (zipfile.c case 4 reads the
		// compressed bytes regardless of method; zipfile2 4.2).
		return e.raw, nil
	case 5:
		if e.isDir() {
			return nil, nil
		}
		// data: unzip-on-read. Unknown compression methods return NULL
		// without error (zipfileColumn's method guard; zipfile2 4.2 expects
		// data IS NULL with method=9).
		switch e.method {
		case 0:
			return e.data, nil
		case 8:
			return e.data, nil
		default:
			return nil, nil
		}
	case 6:
		return int64(e.method), nil
	case 7:
		// z column: cursor context for zipfile_cds() (SQLite passes a
		// live cursor id; this port encodes archive path + entry index).
		return ZipCdsSentinelPrefix + c.src + "\x1f" + strconv.Itoa(c.idx), nil
	}
	return nil, fmt.Errorf("sqlite_zipfile: invalid column index %d", idx)
}

// Close implements Cursor.
func (c *zipCursor) Close() error { return nil }

// --- write support ---

// RowUpdater marks the instance writable.
func (v *zipfileVTab) RowUpdater() {}

// InsertRow appends (or replaces) one member; sz/rawdata must be NULL.
func (v *zipfileVTab) InsertRow(values []interface{}) (int64, error) {
	return v.insertRow(values, "")
}

// InsertRowConflict implements ConflictAwareInserter: REPLACE overwrites a
// same-name entry, IGNORE skips the row silently, other actions keep the
// duplicate-name error.
func (v *zipfileVTab) InsertRowConflict(values []interface{}, resolve string) (int64, error) {
	return v.insertRow(values, resolve)
}

func (v *zipfileVTab) insertRow(values []interface{}, resolve string) (int64, error) {
	get := func(i int) interface{} {
		if i < len(values) {
			return values[i]
		}
		return nil
	}
	name := ""
	if s, ok := get(0).(string); ok {
		name = s
	}
	// zipfile.c accepts a NULL name (stored as ""); no error is raised.
	if get(4) != nil {
		return 0, fmt.Errorf("rawdata must be NULL")
	}
	if get(3) != nil {
		return 0, fmt.Errorf("sz must be NULL")
	}
	mode, mtime, method, data, werr := v.writeParams(get(1), get(2), get(6), get(5))
	if werr != nil {
		return 0, werr
	}
	if get(6) == nil && len(data) > 0 {
		// zipfile.c xUpdate: a NULL method auto-selects deflate only when
		// it actually shrinks the payload; otherwise the entry stays
		// stored (method 0).
		if len(zipDeflate(8, data)) < len(data) {
			method = 8
		} else {
			method = 0
		}
	}
	entries, err := v.loadEntries()
	if err != nil {
		return 0, err
	}
	entry, err := zipFinalizeEntry(name, mode, mtime, method, data)
	if err != nil {
		return 0, err
	}
	for i := range entries {
		// zipfileComparePath treats a trailing slash as insignificant:
		// inserting 'file1' (as a directory) collides with 'file1/'.
		if strings.TrimSuffix(entries[i].name, "/") == strings.TrimSuffix(entry.name, "/") {
			switch resolve {
			case "IGNORE":
				return 0, nil // OR IGNORE: skip silently
			case "REPLACE":
				// Drop the stale entry; the replacement is appended below.
				// Duplicate names are unique up to this point, so scanning
				// further would only miss removals behind mutated indices.
				entries = append(entries[:i], entries[i+1:]...)
			default:
				return 0, fmt.Errorf("duplicate name: %q", entry.name)
			}
		}
	}
	entries = append(entries, entry)
	return 0, v.storeArchive(entries)
}

// UpdateRow applies changes keyed on the original name (column 0).
func (v *zipfileVTab) UpdateRow(oldValues, newValues []interface{}) error {
	return v.updateRow(oldValues, newValues, "")
}

// UpdateRowConflict applies SQLite's statement-level conflict policy to
// zipfile's name-keyed xUpdate operation.
func (v *zipfileVTab) UpdateRowConflict(oldValues, newValues []interface{}, resolve string) error {
	return v.updateRow(oldValues, newValues, resolve)
}

func (v *zipfileVTab) updateRow(oldValues, newValues []interface{}, resolve string) error {
	if len(oldValues) == 0 || len(newValues) == 0 {
		return fmt.Errorf("zipfile: fullname is required")
	}
	oldName, _ := oldValues[0].(string)
	entries, err := v.loadEntries()
	if err != nil {
		return err
	}
	idx := -1
	for i := range entries {
		if entries[i].name == oldName {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("zipfile: no such entry: %s", oldName)
	}
	e := entries[idx]
	setStr := func(i int, dst *string) {
		if i < len(newValues) {
			if s, ok := newValues[i].(string); ok {
				*dst = s
			}
		}
	}
	isNull := func(i int) bool {
		return i < len(newValues) && newValues[i] == interface{}(ExplicitNull{})
	}
	setInt := func(i int, dst *int64) {
		if i < len(newValues) {
			if n, ok := asInt64(newValues[i]); ok {
				*dst = n
			}
		}
	}
	var modeI, mtimeI, methodI int64
	modeI, mtimeI, methodI = int64(e.mode), e.munix, int64(e.method)
	var modeErr error
	setMode := func(i int) {
		if isNull(i) {
			modeI = 0 // explicit NULL: let zipFinalizeEntry default it
			return
		}
		if i < len(newValues) {
			switch m := newValues[i].(type) {
			case string:
				parsed, perr := ZipParseModeText(m)
				if perr != nil {
					modeErr = perr
					return
				}
				modeI = int64(parsed)
			default:
				if n, ok := asInt64(newValues[i]); ok {
					modeI = n
				}
			}
		}
	}
	newName := e.name
	setStr(0, &newName)
	setMode(1)
	if modeErr != nil {
		return modeErr
	}
	setInt(2, &mtimeI)
	setInt(6, &methodI)
	var data []byte
	hasData := false
	if len(newValues) > 5 {
		if str, ok := newValues[5].(string); ok {
			data = []byte(str)
			hasData = true
		}
	}
	if !hasData && e.data == nil {
		data = nil // still a directory entry
	} else if !hasData {
		data = e.data // column untouched: keep existing content
	}
	if len(newValues) > 5 && (newValues[5] == nil || isNull(5)) {
		data = nil // explicit NULL clears the payload (directory entry)
	}
	entry, err := zipFinalizeEntry(newName, uint32(modeI), mtimeI, uint16(methodI), data)
	if err != nil {
		return err
	}
	// zipfileComparePath: renaming onto an existing other entry is a
	// duplicate-name constraint error (zipfile.test 11.6).
	for i := range entries {
		if i == idx || strings.TrimSuffix(entries[i].name, "/") != strings.TrimSuffix(entry.name, "/") {
			continue
		}
		switch strings.ToUpper(resolve) {
		case "IGNORE":
			return nil
		case "REPLACE":
			entries = append(entries[:i], entries[i+1:]...)
			if i < idx {
				idx--
			}
		default:
			return fmt.Errorf("duplicate name: %q", entry.name)
		}
		break
	}
	entries[idx] = entry
	return v.storeArchive(entries)
}

// DeleteRow removes the member with oldValues[0]'s name.
func (v *zipfileVTab) DeleteRow(oldValues []interface{}) error {
	name, _ := oldValues[0].(string)
	entries, err := v.loadEntries()
	if err != nil {
		return err
	}
	out := entries[:0]
	for _, e := range entries {
		if e.name != name {
			out = append(out, e)
		}
	}
	return v.storeArchive(out)
}

// writeParams coerces the INSERT value forms (text mode like '0644', NULLs).
func (v *zipfileVTab) writeParams(modeV, mtimeV, methodV, dataV interface{}) (uint32, int64, uint16, []byte, error) {
	mode := uint32(0)
	switch m := modeV.(type) {
	case string:
		parsed, perr := ZipParseModeText(m)
		if perr != nil {
			return 0, 0, 0, nil, fmt.Errorf("zipfile: parse error in mode: %s", m)
		}
		mode = parsed
	case int64:
		mode = uint32(m)
	}
	var mtime int64
	if n, ok := asInt64(mtimeV); ok {
		mtime = n
	}
	method := uint16(0)
	if n, ok := asInt64(methodV); ok {
		method = uint16(n)
	}
	var data []byte
	switch d := dataV.(type) {
	case string:
		data = []byte(d)
	case []byte:
		data = d
	default:
		// INTEGER/REAL payload: SQLite renders value_text ("10" for 10).
		if n, ok := asInt64(d); ok {
			data = []byte(strconv.FormatInt(n, 10))
		}
	}
	return mode, mtime, method, data, nil
}

// zipFinalizeEntry applies zipfile.c's directory/file consistency rules:
// NULL data marks a directory (name gains a trailing slash), the mode must
// agree with the directory bit, and an absent mode defaults per kind.
func zipFinalizeEntry(name string, mode uint32, mtime int64, method uint16, data []byte) (zipEntry, error) {
	bIsDir := data == nil
	if method != 0 && method != 8 {
		return zipEntry{}, fmt.Errorf("unknown compression method: %d", method)
	}
	if mode == 0 {
		if bIsDir {
			mode = 0040000 + 0755
		} else {
			mode = 0100000 + 0644
		}
	}
	isDirMode := mode&0040000 != 0
	if isDirMode != bIsDir {
		return zipEntry{}, fmt.Errorf("zipfile: mode does not match data")
	}
	if bIsDir && !strings.HasSuffix(name, "/") {
		name += "/"
	}
	if bIsDir {
		data = nil
	}
	return newZipEntry(name, mode, mtime, method, data), nil
}

// ZipParseModeText converts a TCL/zipfile mode string ("-rw-r--r--") to a
// unix mode (0100644); octal strings pass through ParseUint.
func ZipParseModeText(m string) (uint32, error) {
	if len(m) == 10 && (m[0] == '-' || m[0] == 'd') {
		var perm uint32
		for i := 1; i < 10; i++ {
			switch m[i] {
			case 'r', 'w', 'x':
				perm |= 1 << uint(9-i)
			}
		}
		typ := uint32(0100000)
		if m[0] == 'd' {
			typ = 0040000
		}
		return typ | perm, nil
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(m, "0"), 8, 32)
	if err != nil {
		return 0, fmt.Errorf("zipfile: parse error in mode: %s", m)
	}
	return uint32(n), nil
}

// newZipEntry fills derived fields (crc, dos time).
func newZipEntry(name string, mode uint32, mtime int64, method uint16, data []byte) zipEntry {
	dd, dt := zipDosFromUnix(mtime)
	return zipEntry{
		name: name, mode: mode, munix: mtime, method: method,
		data: data, crc: crc32.ChecksumIEEE(data),
		dosTime: dt, dosDate: dd, hasUTStamp: true,
	}
}

// storeArchive serializes entries back to the bound source using the same
// record layout as zipfile.c (LFH+CDF with 9-byte extended-timestamp extra).
func (v *zipfileVTab) storeArchive(entries []zipEntry) error {
	out := zipSerialize(entries)
	if v.filePath != "" {
		if err := os.WriteFile(v.filePath, out, 0644); err != nil {
			// zipfile.c: fopen(zFile, "ab+") failure — e.g. the path is a
			// directory (zipfile.test 8.1.2/8.2.2).
			return fmt.Errorf("zipfile: failed to open file %s for writing", v.filePath)
		}
		return nil
	}
	zipBlobSave(v.dataArg, string(out))
	v.dataArg = string(out)
	return nil
}

// zipSerialize renders members as a complete zip archive image
// (local headers + central directory + EOCD).
func zipSerialize(entries []zipEntry) []byte {
	var buf bytes.Buffer
	type cdsOff struct {
		entry  zipEntry
		offset int
	}
	var cdss []cdsOff
	for _, e := range entries {
		comp := zipDeflate(e.method, e.data)
		extra := zipUTExtra(e.munix)
		off := buf.Len()
		// Local file header.
		buf.Write(u32le(0x04034b50))
		buf.Write(u16le(20))                  // version needed
		buf.Write(u16le(0x800))               // flags: UTF-8 names
		buf.Write(u16le(e.method))            //
		buf.Write(u16le(e.dosTime))           //
		buf.Write(u16le(e.dosDate))           //
		buf.Write(u32le(e.crc))               //
		buf.Write(u32le(uint32(len(comp))))   // compressed size
		buf.Write(u32le(uint32(len(e.data)))) // uncompressed size
		buf.Write(u16le(uint16(len(e.name)))) //
		buf.Write(u16le(uint16(len(extra))))  //
		buf.WriteString(e.name)               //
		buf.Write(extra)                      //
		buf.Write(comp)                       //
		cdss = append(cdss, cdsOff{e, off})   //
	}
	cdStart := buf.Len()
	for _, co := range cdss {
		e := co.entry
		comp := zipDeflate(e.method, e.data)
		extra := zipUTExtra(e.munix)
		buf.Write(u32le(0x02014b50))
		buf.Write(u16le((3 << 8) + 30))       // version made by
		buf.Write(u16le(20))                  // version needed
		buf.Write(u16le(0x800))               // flags
		buf.Write(u16le(e.method))            //
		buf.Write(u16le(e.dosTime))           //
		buf.Write(u16le(e.dosDate))           //
		buf.Write(u32le(e.crc))               //
		buf.Write(u32le(uint32(len(comp))))   //
		buf.Write(u32le(uint32(len(e.data)))) //
		buf.Write(u16le(uint16(len(e.name)))) //
		buf.Write(u16le(uint16(len(extra))))  //
		buf.Write(u16le(0))                   // comment len
		buf.Write(u16le(0))                   // disk start
		buf.Write(u16le(0))                   // internal attrs
		buf.Write(u32le(e.mode << 16))        // external attrs
		buf.Write(u32le(uint32(co.offset)))   // local header offset
		buf.WriteString(e.name)
		buf.Write(extra)
	}
	cdSize := buf.Len() - cdStart
	buf.Write(u32le(0x06054b50))
	buf.Write(u16le(0)) // disk
	buf.Write(u16le(0)) // first disk
	buf.Write(u16le(uint16(len(cdss))))
	buf.Write(u16le(uint16(len(cdss))))
	buf.Write(u32le(uint32(cdSize)))
	buf.Write(u32le(uint32(cdStart)))
	buf.Write(u16le(0)) // comment len

	return buf.Bytes()
}

func zipUTTimestamp(extra []byte) (int64, bool) {
	for i := 0; i+9 <= len(extra); {
		id := binary.LittleEndian.Uint16(extra[i : i+2])
		n := int(binary.LittleEndian.Uint16(extra[i+2 : i+4]))
		if id == 0x5455 && n >= 5 {
			return int64(binary.LittleEndian.Uint32(extra[i+5 : i+9])), true
		}
		if i+4+n > len(extra) {
			break
		}
		i += 4 + n
	}
	return 0, false
}

func zipUTExtra(munix int64) []byte {
	b := make([]byte, 9)
	binary.LittleEndian.PutUint16(b[0:2], 0x5455)
	binary.LittleEndian.PutUint16(b[2:4], 5)
	b[4] = 1
	binary.LittleEndian.PutUint32(b[5:9], uint32(munix))
	return b
}

func u16le(v uint16) []byte { b := make([]byte, 2); binary.LittleEndian.PutUint16(b, v); return b }
func u32le(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }

// WithoutRowidVTab marks the schema WITHOUT ROWID.
func (v *zipfileVTab) WithoutRowid() bool { return true }

// ZipScalar builds a single-entry archive for the zipfile() SQL scalar
// function (zipfile.c's multi-argument scalar form).
func ZipScalar(name string, mtime int64, data []byte, method uint16, mode uint32) ([]byte, error) {
	if method != 0 && method != 8 {
		return nil, fmt.Errorf("illegal method value: %d", method)
	}
	isDir := data == nil
	if !isDir && strings.HasSuffix(name, "/") {
		return nil, fmt.Errorf("non-directory name must not end with /")
	}
	e := newZipEntry(name, mode, mtime, method, data)
	v := &zipfileVTab{dataArg: ""}
	_ = v
	var buf bytes.Buffer
	comp := zipDeflate(e.method, e.data)
	extra := zipUTExtra(e.munix)
	buf.Write(u32le(0x04034b50))
	buf.Write(u16le(20))
	buf.Write(u16le(0x800))
	buf.Write(u16le(e.method))
	buf.Write(u16le(e.dosTime))
	buf.Write(u16le(e.dosDate))
	buf.Write(u32le(e.crc))
	buf.Write(u32le(uint32(len(comp))))
	buf.Write(u32le(uint32(len(e.data))))
	buf.Write(u16le(uint16(len(e.name))))
	buf.Write(u16le(uint16(len(extra))))
	buf.WriteString(e.name)
	buf.Write(extra)
	buf.Write(comp)
	off := buf.Len()
	buf.Write(u32le(0x02014b50))
	buf.Write(u16le((3 << 8) + 30))
	buf.Write(u16le(20))
	buf.Write(u16le(0x800))
	buf.Write(u16le(e.method))
	buf.Write(u16le(e.dosTime))
	buf.Write(u16le(e.dosDate))
	buf.Write(u32le(e.crc))
	buf.Write(u32le(uint32(len(comp))))
	buf.Write(u32le(uint32(len(e.data))))
	buf.Write(u16le(uint16(len(e.name))))
	buf.Write(u16le(uint16(len(extra))))
	buf.Write(u16le(0))
	buf.Write(u16le(0))
	buf.Write(u16le(0))
	buf.Write(u32le(e.mode << 16))
	buf.Write(u32le(uint32(off)))
	buf.WriteString(e.name)
	buf.Write(extra)
	cdSize := buf.Len() - off
	buf.Write(u32le(0x06054b50))
	buf.Write(u16le(0))
	buf.Write(u16le(0))
	buf.Write(u16le(1))
	buf.Write(u16le(1))
	buf.Write(u32le(uint32(cdSize)))
	buf.Write(u32le(uint32(off)))
	buf.Write(u16le(0))
	return buf.Bytes(), nil
}

// ZipCdsSentinelPrefix marks a z-column value carrying cursor context for
// the zipfile_cds() overload. SQLite passes a live cursor id through
// xFindFunction; this port materializes rows eagerly, so the context is
// encoded into the value itself (archive path + entry index).
const ZipCdsSentinelPrefix = "\x1fzipcds:"

// ZipCdsJSON rebuilds the central-directory-structure JSON that
// zipfile.c's zipfile_cds() returns for one archive member.
func ZipCdsJSON(path string, idx int) interface{} {
	v := &zipfileVTab{filePath: path}
	entries, err := v.loadEntries()
	if err != nil || idx < 0 || idx >= len(entries) {
		return nil
	}
	e := entries[idx]
	return fmt.Sprintf(`{"version-made-by":%d,"version-to-extract":%d,"flags":%d,"compression":%d,"time":%d,"date":%d,"crc32":%d,"compressed-size":%d,"uncompressed-size":%d,"file-name-length":%d,"extra-field-length":%d,"file-comment-length":0,"disk-number-start":0,"internal-attr":0,"external-attr":%d,"offset":0}`,
		3<<8|30, 20, 0x800, e.method, e.dosTime, e.dosDate, e.crc,
		len(zipDeflate(e.method, e.data)), len(e.data), len(e.name), 9, e.mode<<16)
}

// ZipEntrySpec is one member accumulated by the zipfile() aggregate.
type ZipEntrySpec struct {
	Name   string
	Mode   uint32
	Mtime  int64
	Method uint16
	Data   []byte
}

// zipMemCeiling mirrors SQLite's largest single allocation (sqlite3Malloc
// rejects nByte above 0x7fffff00 with SQLITE_NOMEM): assembling an archive
// whose members exceed this cumulative staging size fails with "out of
// memory" before any member payload is deflated (zipfile.test 23.0).
const zipMemCeiling = int64(0x7fffff00)

// ZipAgg implements the zipfile() aggregate (zipfile.c zipStep/xFinal):
// each input row contributes one member and Final serializes the combined
// archive. As an aggregate it yields exactly ONE blob per group — the
// source of SQLite's INSERT INTO t SELECT zipfile(...) FROM t row counts.
type ZipAgg struct {
	entries    []ZipEntrySpec
	stagedZero int64 // cumulative declared size of zeroblob members seen so far
}

// Step accumulates one member, validating like zipStep.
func (z *ZipAgg) Step(args []interface{}) error {
	// zipStep accepts exactly the 2-, 4-, and 5-argument forms.
	if len(args) != 2 && len(args) != 4 && len(args) != 5 {
		return fmt.Errorf("wrong number of arguments to function zipfile()")
	}
	if args[0] == nil {
		return fmt.Errorf("first argument to zipfile() must be non-NULL")
	}
	name, _ := args[0].(string)
	get := func(i int) interface{} {
		if i < len(args) {
			return args[i]
		}
		return nil
	}
	modeArg, mtimeArg, methodArg, dataArg := interface{}(nil), interface{}(nil), interface{}(nil), get(1)
	if len(args) >= 4 {
		modeArg, mtimeArg, dataArg = get(1), get(2), get(3)
		methodArg = get(4)
	}
	mode := uint32(0) // zipFinalizeEntry defaults by kind
	switch mv := modeArg.(type) {
	case string:
		parsed, perr := ZipParseModeText(mv)
		if perr != nil {
			return fmt.Errorf("zipfile: parse error in mode: %s", mv)
		}
		mode = parsed
	default:
		if modeArg != nil {
			if n, ok := AsVtabInt64(modeArg); ok {
				mode = uint32(n)
			}
		}
	}
	var mtime int64
	if mtimeArg != nil {
		if n, ok := AsVtabInt64(mtimeArg); ok {
			mtime = n
		}
	}
	method := uint16(0)
	if methodArg != nil {
		n, ok := AsVtabInt64(methodArg)
		if !ok {
			return fmt.Errorf("illegal method value: %v", methodArg)
		}
		method = uint16(n)
		if method != 0 && method != 8 {
			return fmt.Errorf("illegal method value: %d", n)
		}
	}
	var data []byte
	switch d := dataArg.(type) {
	case string:
		data = []byte(d)
	case []byte:
		data = d
	case value.ZeroBlob:
		// zeroblob(N) members stage lazily in SQLite: only the declared size
		// matters until the archive is assembled. Crossing SQLite's largest
		// single allocation fails the statement with NOMEM ("out of memory")
		// exactly as sqlite3VdbeMemExpandBlob would.
		z.stagedZero += int64(d.N)
		if z.stagedZero > zipMemCeiling {
			return fmt.Errorf("out of memory")
		}
		if d.N > 0 {
			data = make([]byte, d.N) // zero-filled by allocation semantics
		}
	default:
		if d != nil {
			if n, ok := AsVtabInt64(d); ok {
				data = []byte(strconv.FormatInt(n, 10))
			}
		}
	}
	if methodArg == nil && len(data) > 0 && len(zipDeflate(8, data)) < len(data) {
		method = 8
	}
	if data != nil && len(name) > 0 && name[len(name)-1] == '/' {
		return fmt.Errorf("non-directory name must not end with /")
	}
	entry, err := zipFinalizeEntry(name, mode, mtime, method, data)
	if err != nil {
		return err
	}
	z.entries = append(z.entries, ZipEntrySpec{
		Name: entry.name, Mode: entry.mode, Mtime: entry.munix,
		Method: entry.method, Data: entry.data,
	})
	return nil
}

// Final serializes the accumulated members into one archive blob.
func (z *ZipAgg) Final() (interface{}, error) {
	entries := make([]zipEntry, 0, len(z.entries))
	for _, s := range z.entries {
		entries = append(entries, newZipEntry(s.Name, s.Mode, s.Mtime, s.Method, s.Data))
	}
	out := zipSerialize(entries)
	if len(out) > 1<<30 {
		// C hits SQLITE_NOMEM assembling giant archives.
		return nil, fmt.Errorf("out of memory")
	}
	return out, nil
}
