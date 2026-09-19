package vtab

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/value"
)

// This file holds zipfile's write path (ext/misc/zipfile.c xUpdate +
// zipStep/xFinal): insert/update/delete of archive members, entry
// finalization, archive serialization and the zipfile() scalar/aggregate
// builders. Reading lives in zipfile.go.

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
		method = zipAutoMethod(data)
	}
	entries, err := v.loadEntries()
	if err != nil {
		return 0, err
	}
	entry, err := zipFinalizeEntry(name, mode, mtime, method, data)
	if err != nil {
		return 0, err
	}
	entries, skip, rerr := zipResolveInsert(entries, entry, resolve)
	if rerr != nil || skip {
		return 0, rerr
	}
	entries = append(entries, entry)
	return 0, v.storeArchive(entries)
}

// zipAutoMethod picks the method for a NULL-method insert: deflate when it
// shrinks the payload, stored otherwise (zipfile.c xUpdate).
func zipAutoMethod(data []byte) uint16 {
	if len(zipDeflate(8, data)) < len(data) {
		return 8
	}
	return 0
}

// zipResolveInsert applies the conflict policy when an inserted entry's name
// already exists (zipfileComparePath treats a trailing slash as
// insignificant). skip reports an OR IGNORE no-op. The scan keeps the C
// loop's bound: the entry count is captured before any REPLACE removal.
func zipResolveInsert(entries []zipEntry, entry zipEntry, resolve string) ([]zipEntry, bool, error) {
	n := len(entries)
	for i := 0; i < n; i++ {
		if strings.TrimSuffix(entries[i].name, "/") != strings.TrimSuffix(entry.name, "/") {
			continue
		}
		switch resolve {
		case "IGNORE":
			return entries, true, nil // OR IGNORE: skip silently
		case "REPLACE":
			// Drop the stale entry; the replacement is appended below.
			// Duplicate names are unique up to this point, so scanning
			// further would only miss removals behind mutated indices.
			entries = append(entries[:i], entries[i+1:]...)
		default:
			return entries, false, fmt.Errorf("duplicate name: %q", entry.name)
		}
	}
	return entries, false, nil
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
	idx := zipFindEntry(entries, oldName)
	if idx < 0 {
		return fmt.Errorf("zipfile: no such entry: %s", oldName)
	}
	entry, err := zipUpdatedEntry(entries[idx], newValues)
	if err != nil {
		return err
	}
	return v.applyUpdate(entries, idx, entry, resolve)
}

// zipFindEntry locates an entry by exact name; -1 when absent.
func zipFindEntry(entries []zipEntry, name string) int {
	for i := range entries {
		if entries[i].name == name {
			return i
		}
	}
	return -1
}

// zipUpdatedEntry applies the new column values to a stored entry
// (zipfileUpdateMethod's field coercion): absent columns keep the stored
// values, an explicit NULL mode re-defaults, an explicit NULL payload clears
// the entry to a directory.
func zipUpdatedEntry(e zipEntry, newValues []interface{}) (zipEntry, error) {
	newName, ok := colString(newValues, 0)
	if !ok {
		newName = e.name
	}
	mode, err := zipUpdatedMode(e.mode, newValues)
	if err != nil {
		return zipEntry{}, err
	}
	mtimeI, methodI := e.munix, int64(e.method)
	colInt(newValues, 2, &mtimeI)
	colInt(newValues, 6, &methodI)
	data := zipUpdatedData(e, newValues)
	return zipFinalizeEntry(newName, mode, mtimeI, uint16(methodI), data)
}

// colString reads values[i] as a string; ok=false when absent or not text.
func colString(values []interface{}, i int) (string, bool) {
	if i < len(values) {
		if s, ok := values[i].(string); ok {
			return s, true
		}
	}
	return "", false
}

// colInt applies values[i] to *dst when it holds an integer.
func colInt(values []interface{}, i int, dst *int64) {
	if i < len(values) {
		if n, ok := asInt64(values[i]); ok {
			*dst = n
		}
	}
}

// isExplicitNull reports whether values[i] is an explicit SQL NULL marker.
func isExplicitNull(values []interface{}, i int) bool {
	return i < len(values) && values[i] == interface{}(ExplicitNull{})
}

// zipUpdatedMode coerces the mode column: a text mode ("-rw-r--r--") parses,
// an explicit NULL re-defaults (0), integers pass through; an absent or
// non-numeric column keeps the stored mode.
func zipUpdatedMode(old uint32, newValues []interface{}) (uint32, error) {
	if len(newValues) <= 1 {
		return old, nil
	}
	if isExplicitNull(newValues, 1) {
		return 0, nil // explicit NULL: let zipFinalizeEntry default it
	}
	switch m := newValues[1].(type) {
	case string:
		parsed, perr := ZipParseModeText(m)
		if perr != nil {
			return 0, perr
		}
		return parsed, nil
	default:
		if n, ok := asInt64(newValues[1]); ok {
			return uint32(n), nil
		}
	}
	return old, nil
}

// zipUpdatedData resolves the updated payload: a string column replaces it,
// an explicit NULL (or SQL NULL) clears it to a directory entry, an untouched
// column keeps the stored content.
func zipUpdatedData(e zipEntry, newValues []interface{}) []byte {
	if len(newValues) > 5 {
		if str, ok := newValues[5].(string); ok {
			return []byte(str)
		}
		if newValues[5] == nil || isExplicitNull(newValues, 5) {
			return nil // explicit NULL clears the payload (directory entry)
		}
	}
	if e.data == nil {
		return nil // still a directory entry
	}
	return e.data // column untouched: keep existing content
}

// applyUpdate replaces entries[idx] with entry, applying the duplicate-name
// policy: renaming onto an existing other entry is a constraint error unless
// IGNORE/REPLACE resolves it (zipfileComparePath, zipfile.test 11.6).
func (v *zipfileVTab) applyUpdate(entries []zipEntry, idx int, entry zipEntry, resolve string) error {
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
	if bIsDir {
		// zipfile.c zipfileStep: "If this is a directory entry, ensure
		// that there is exactly one '/' at the end of the path." A name
		// without one gains it; duplicate trailing slashes collapse
		// ("dir3//" stores as "dir3/"), keeping a bare "/" intact.
		if !strings.HasSuffix(name, "/") {
			name += "/"
		} else {
			for len(name) > 1 && name[len(name)-2] == '/' {
				name = name[:len(name)-1]
			}
		}
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
	mode, mtime, method, data, hasMethod, err := z.zipAggParams(args)
	if err != nil {
		return err
	}
	method = zipAggAutoMethod(method, data, hasMethod)
	name, _ := args[0].(string)
	if err := zipCheckName(name, data); err != nil {
		return err
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

// zipAggAutoMethod applies the NULL-method auto-selection: deflate only when
// it shrinks the payload (zipStep).
func zipAggAutoMethod(method uint16, data []byte, hasMethod bool) uint16 {
	if hasMethod || len(data) == 0 || len(zipDeflate(8, data)) >= len(data) {
		return method
	}
	return 8
}

// zipCheckName rejects a payload under a directory-style name (zipStep).
func zipCheckName(name string, data []byte) error {
	if data != nil && len(name) > 0 && name[len(name)-1] == '/' {
		return fmt.Errorf("non-directory name must not end with /")
	}
	return nil
}

// zipAggParams coerces one zipfile() aggregate argument row (zipStep's field
// pass): the 2-argument form carries only the payload, the 4-/5-argument
// forms add mode, mtime and method.
func (z *ZipAgg) zipAggParams(args []interface{}) (mode uint32, mtime int64, method uint16, data []byte, hasMethod bool, err error) {
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
	mode, err = zipAggMode(modeArg)
	if err != nil {
		return
	}
	if mtimeArg != nil {
		if n, ok := AsVtabInt64(mtimeArg); ok {
			mtime = n
		}
	}
	method, err = zipAggMethod(methodArg)
	if err != nil {
		return
	}
	hasMethod = methodArg != nil
	data, err = z.zipAggData(dataArg)
	return
}

// zipAggMode coerces the mode argument: a text mode parses, an integer
// passes, nil (or a non-numeric value) defaults to 0 — zipFinalizeEntry then
// defaults by kind.
func zipAggMode(modeArg interface{}) (uint32, error) {
	switch mv := modeArg.(type) {
	case string:
		parsed, perr := ZipParseModeText(mv)
		if perr != nil {
			return 0, fmt.Errorf("zipfile: parse error in mode: %s", mv)
		}
		return parsed, nil
	default:
		if modeArg != nil {
			if n, ok := AsVtabInt64(modeArg); ok {
				return uint32(n), nil
			}
		}
	}
	return 0, nil
}

// zipAggMethod coerces and validates the method argument: only 0 (stored)
// and 8 (deflate) are legal.
func zipAggMethod(methodArg interface{}) (uint16, error) {
	if methodArg == nil {
		return 0, nil
	}
	n, ok := AsVtabInt64(methodArg)
	if !ok {
		return 0, fmt.Errorf("illegal method value: %v", methodArg)
	}
	method := uint16(n)
	if method != 0 && method != 8 {
		return 0, fmt.Errorf("illegal method value: %d", n)
	}
	return method, nil
}

// zipAggData coerces the payload argument. zeroblob(N) members stage lazily
// in SQLite: only the declared size matters until the archive is assembled.
// Crossing SQLite's largest single allocation fails the statement with NOMEM
// ("out of memory") exactly as sqlite3VdbeMemExpandBlob would.
func (z *ZipAgg) zipAggData(dataArg interface{}) ([]byte, error) {
	switch d := dataArg.(type) {
	case string:
		return []byte(d), nil
	case []byte:
		return d, nil
	case value.ZeroBlob:
		z.stagedZero += int64(d.N)
		if z.stagedZero > zipMemCeiling {
			return nil, fmt.Errorf("out of memory")
		}
		if d.N > 0 {
			return make([]byte, d.N), nil // zero-filled by allocation semantics
		}
		return nil, nil
	default:
		if d != nil {
			if n, ok := AsVtabInt64(d); ok {
				return []byte(strconv.FormatInt(n, 10)), nil
			}
		}
	}
	return nil, nil
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
