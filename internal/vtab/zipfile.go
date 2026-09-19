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
	// The argument may be "name = value" form (CREATE VIRTUAL TABLE)
	// or a bare value (table-function call).
	a := strings.TrimSpace(args[0])
	if eq := strings.Index(a, "="); eq >= 0 && !strings.ContainsAny(a[:eq], "/.") {
		a = strings.TrimSpace(a[eq+1:])
	}
	a = unquoteVtabArg(a)
	if !looksLikeFilePath(a) {
		v.dataArg = a
		return v, nil
	}
	if err := v.bindFilePath(a, createOK); err != nil {
		return nil, err
	}
	return v, nil
}

// bindFilePath resolves a file-path archive source (zipfile.c's fopen path):
// a directory connects as an empty archive; a missing file is created only
// by the CREATE VIRTUAL TABLE form.
func (v *zipfileVTab) bindFilePath(a string, createOK bool) error {
	if st, serr := os.Stat(a); serr == nil && st.IsDir() {
		// SQLite's zipfile opens the archive lazily; a directory
		// connects fine (empty central directory) and only the
		// first write fails (zipfile.test 8.1.x).
		v.filePath = a
		return nil
	}
	if _, serr := os.Stat(a); serr != nil {
		if !createOK {
			// The table-valued form reads an EXISTING archive;
			// only the CREATE VIRTUAL TABLE form creates one
			// (zipfile.test 19.x).
			return fmt.Errorf("error in zipfile module: cannot open file: %s", a)
		}
		f, ferr := os.OpenFile(a, os.O_CREATE|os.O_RDWR, 0644)
		if ferr != nil {
			return fmt.Errorf("error in zipfile module: cannot open file: %s", a)
		}
		f.Close()
	}
	v.filePath = a
	return nil
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
	n, off, err := zipCentralDirectory(raw)
	if err != nil {
		return nil, err
	}
	var out []zipEntry
	for i := 0; i < n; i++ {
		e, next, perr := zipParseEntry(raw, off)
		if perr != nil {
			return nil, perr
		}
		out = append(out, e)
		off = next
	}
	return out, nil
}

// zipCentralDirectory locates the EOCD and returns the entry count and the
// central-directory offset (zipfileReadEOCD + zipfileLoadDirectory's
// prologue).
func zipCentralDirectory(raw []byte) (n, off int, err error) {
	eocd := bytes.LastIndex(raw, []byte{0x50, 0x4b, 0x05, 0x06})
	if eocd < 0 {
		if len(raw) == 0 {
			return 0, 0, nil
		}
		// zipfileReadEOCD reports this defect verbatim; the engine adds no
		// "error in zipfile module:" prefix to it (zipfile2 4.3.*).
		return 0, 0, fmt.Errorf("cannot find end of central directory record")
	}
	n = int(binary.LittleEndian.Uint16(raw[eocd+10 : eocd+12]))
	off = int(binary.LittleEndian.Uint32(raw[eocd+16 : eocd+20]))
	// zipfile.c zipfileLoadDirectory: a central directory that claims
	// entries outside the image is corruption (zipfile.test 17.x).
	if n > 0 && (off < 0 || off+46 > len(raw) || binary.LittleEndian.Uint32(raw[off:off+4]) != 0x02014b50) {
		return 0, 0, fmt.Errorf("error in zipfile module: zip archive is corrupt")
	}
	return n, off, nil
}

// zipParseEntry parses one central-directory record at off, returning the
// entry and the offset of the next record (zipfileLoadDirectory's body).
func zipParseEntry(raw []byte, off int) (zipEntry, int, error) {
	// zipfile.c requires EVERY declared central-directory record to be
	// present with a valid signature; a truncated or mis-signed record is
	// corruption, never a silent truncation (zipfile2 3.3 patched PK
	// signatures must error).
	if off+46 > len(raw) || binary.LittleEndian.Uint32(raw[off:off+4]) != 0x02014b50 {
		return zipEntry{}, 0, fmt.Errorf("error in zipfile module: zip archive is corrupt")
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
		return zipEntry{}, 0, fmt.Errorf("error in zipfile module: zip archive is corrupt")
	}
	e.name = string(raw[off+46 : off+46+nName])
	e.applyUTTimestamps(raw, off, nName, nExtra, lho)
	if err := e.readLFHPayload(raw, off, szComp, lho); err != nil {
		return zipEntry{}, 0, err
	}
	return e, off + 46 + nName + nExtra, nil
}

// applyUTTimestamps resolves the entry's unix mtime: the central-directory
// extended timestamp (0x5455), else the local header's, else the DOS fields.
func (e *zipEntry) applyUTTimestamps(raw []byte, off, nName, nExtra, lho int) {
	// Extended timestamp (0x5455) in the central directory carries the
	// unix mtime; fall back to decoding the DOS fields.
	e.munix = dosToUnix(e.dosDate, e.dosTime)
	extra := raw[off+46+nName : off+46+nName+nExtra]
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
}

// readLFHPayload locates the member payload behind the local header and
// decompresses it (zipfileReadLFH + zipfileDecompress). A payload running
// past the image end leaves raw/data unset (the entry still scans).
func (e *zipEntry) readLFHPayload(raw []byte, off, szComp, lho int) error {
	// zipfile.c reads the LFH with a signed 32-bit offset; an offset
	// that is negative or past the image is a read failure
	// ("failed to read LFH at offset %lld", zipfile.test 17.x).
	signedLho := int64(int32(uint32(lho)))
	if signedLho < 0 || lho+30 > len(raw) {
		return fmt.Errorf("error in zipfile module: failed to read LFH at offset %d", signedLho)
	}
	// zipfileReadLFH verifies the local-header magic and reports the
	// read failure with the record's offset (zipfile2 3.3/8.x patched
	// signatures must fail with this message).
	if binary.LittleEndian.Uint32(raw[lho:lho+4]) != 0x04034b50 {
		return fmt.Errorf("error in zipfile module: failed to read LFH at offset %d", signedLho)
	}
	nLN := int(binary.LittleEndian.Uint16(raw[lho+26 : lho+28]))
	nLX := int(binary.LittleEndian.Uint16(raw[lho+28 : lho+30]))
	start := lho + 30 + nLN + nLX
	end := start + szComp
	if end > len(raw) {
		return nil
	}
	payload := raw[start:end]
	e.raw = payload
	switch e.method {
	case 0, 8:
		// zipfile.c decompresses during cursor reads and surfaces zlib
		// failures as statement errors (zipfile2 4.1: a patched deflate
		// stream must fail the SELECT with "inflate() failed").
		data, err := zipInflate(e.method, payload, e.crc, int(binary.LittleEndian.Uint32(raw[off+24:off+28])))
		if err != nil {
			return err
		}
		e.data = data
	default:
		// Unknown methods keep only the stored payload: sqlite's data
		// column returns NULL for them (zipfileColumn's method guard),
		// while rawdata still exposes e.raw (zipfile2 4.2).
		e.data = nil
	}
	return nil
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

// zipColumnFuncs maps the scalar columns to their readers (zipfileColumn).
var zipColumnFuncs = [...]func(e *zipEntry) interface{}{
	0: func(e *zipEntry) interface{} { return zipEntryName(e.name) },
	1: func(e *zipEntry) interface{} { return int64(e.mode) },
	2: func(e *zipEntry) interface{} { return e.munix },
	3: func(e *zipEntry) interface{} { return int64(len(e.data)) },
	6: func(e *zipEntry) interface{} { return int64(e.method) },
}

// Column implements Cursor.
func (c *zipCursor) Column(idx int) (interface{}, error) {
	if c.idx < 0 || c.idx >= len(c.entries) {
		return nil, fmt.Errorf("no row")
	}
	if idx < len(zipColumnFuncs) && zipColumnFuncs[idx] != nil {
		return zipColumnFuncs[idx](&c.entries[c.idx]), nil
	}
	return c.computedColumn(idx)
}

// computedColumn reads the state-dependent columns: the rawdata/data pair
// and the z cursor context.
func (c *zipCursor) computedColumn(idx int) (interface{}, error) {
	e := &c.entries[c.idx]
	switch idx {
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
		return zipEntryData(e), nil
	case 7:
		// z column: cursor context for zipfile_cds() (SQLite passes a
		// live cursor id; this port encodes archive path + entry index).
		return ZipCdsSentinelPrefix + c.src + "\x1f" + strconv.Itoa(c.idx), nil
	}
	return nil, fmt.Errorf("sqlite_zipfile: invalid column index %d", idx)
}

// zipEntryName renders the entry name (zipfile.c stores the entry name via
// sqlite3_mprintf("%.*s"), so a name embedding NUL truncates at the first
// NUL byte when returned as TEXT — zipfile.test 22.x crafted archive:
// "A\0BBB…" reads as "A").
func zipEntryName(name string) string {
	if i := strings.IndexByte(name, 0); i >= 0 {
		return name[:i]
	}
	return name
}

// zipEntryData returns the data column: the unzip-on-read payload; unknown
// compression methods return NULL without error (zipfileColumn's method
// guard; zipfile2 4.2 expects data IS NULL with method=9).
func zipEntryData(e *zipEntry) interface{} {
	switch e.method {
	case 0, 8:
		return e.data
	default:
		return nil
	}
}

// Close implements Cursor.
func (c *zipCursor) Close() error { return nil }
