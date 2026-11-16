// Package pager — the super-journal ("master journal") of a multi-file
// commit (pager.c readSuperJournal/writeSuperJournal + pager_delsuper, and
// vdbeaux.c sqlite3VdbeCommit's nTrans gate).
//
// Format contracts (all integers BIG-endian, pager.c read32bits/write32bits):
//
//   - Each participating journal carries a trailing POINTER block appended by
//     writeSuperJournal during CommitPhaseOne:
//	     [4-byte SJ page-number sentinel (PAGER_SJ_PGNO)][name bytes]
//	     [4-byte name length][4-byte checksum (sum of the name bytes)]
//	     [8-byte aJournalMagic]
//     readSuperJournal reads it from the FILE TAIL: length at szJ-16,
//     checksum at szJ-12, magic at szJ-8, name at szJ-16-len.
//
//   - The super-journal file itself is the concatenated NUL-terminated
//     journal names of every write-transaction database in the commit
//     (vdbeaux.c sqlite3VdbeCommit).
//
// Atomicity: a hot journal whose pointer names a super-journal that still
// EXISTS must roll back (the transaction was not committed); once the
// super-journal is deleted the journals are stale (the transaction
// committed) — deleting the super-journal LAST is the commit point.
package pager

import (
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
)

// SuperJournalSuffix renders C's "-mj%06X9%02X": six random hex digits, the
// fixed antepenultimate '9' (an 8+3-filename collision guard), two more
// random hex digits. Exported for the engine's multi-file commit name
// selection (vdbeaux.c sqlite3VdbeCommit).
func SuperJournalSuffix() string {
	var r uint32
	binary.LittleEndian.PutUint32(randBuf[:], 0)
	crand.Read(randBuf[:])
	r = binary.LittleEndian.Uint32(randBuf[:])
	return fmt.Sprintf("-mj%06X9%02X", (r>>8)&0xffffff, r&0xff)
}

// JournalNameFor is sqlite3BtreeGetJournalname: the journal sidecar path of
// a database file (the name written into the super-journal's list).
func JournalNameFor(dbPath string) string {
	return dbPath + "-journal"
}

var randBuf [4]byte

// SuperJournalPointerFromData extracts the trailing super-journal pointer of
// a journal image (pager.c readSuperJournal). ok is false when the file is
// too short, the magic does not match, or the name checksum fails — a
// checksum failure reports a nul name (C: "definitely roll back" via the
// empty-name path).
func SuperJournalPointerFromData(data []byte) (string, bool) {
	if len(data) < 16 {
		return "", false
	}
	szJ := int64(len(data))
	nameLen := int64(binary.BigEndian.Uint32(data[szJ-16:]))
	if nameLen <= 0 || nameLen > szJ-16 {
		return "", false
	}
	cksum := binary.BigEndian.Uint32(data[szJ-12:])
	var magic [8]byte
	copy(magic[:], data[szJ-8:])
	if magic != journalMagicArray {
		return "", false
	}
	name := string(data[szJ-16-nameLen : szJ-16])
	for i := 0; i < len(name); i++ {
		cksum -= uint32(name[i])
	}
	if cksum != 0 {
		return "", false
	}
	return name, true
}

// journalMagicArray is the aJournalMagic bytes as a comparable array.
var journalMagicArray = [8]byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

// AppendSuperJournalPointer appends the super-journal pointer block to this
// pager's journal sidecar (pager.c writeSuperJournal, called from
// sqlite3PagerCommitPhaseOne with the commit's super-journal name): the
// PAGER_SJ_PGNO sentinel stops the hot-journal record walk from parsing the
// pointer block as a page record. Caller holds p.mu (the journal file and
// the append cursor are pager state).
func (p *Pager) AppendSuperJournalPointer(jpath, superName string) error {
	if p.journalFile == nil {
		return nil
	}
	cksum := uint32(0)
	for i := 0; i < len(superName); i++ {
		cksum += uint32(superName[i])
	}
	block := make([]byte, 4+len(superName)+4+4+8)
	binary.BigEndian.PutUint32(block[0:4], p.pendingBytePageFor())
	copy(block[4:], superName)
	binary.BigEndian.PutUint32(block[4+len(superName):], uint32(len(superName)))
	binary.BigEndian.PutUint32(block[8+len(superName):], cksum)
	copy(block[12+len(superName):], journalMagic[:])
	if _, err := p.journalFile.WriteAt(block, p.journalAppendOff); err != nil {
		return fmt.Errorf("pager: super-journal pointer: %w", err)
	}
	p.journalAppendOff += int64(len(block))
	return nil
}

// readSuperJournalNames reads the super-journal's journal-name list (the
// concatenated NUL-terminated journal names written by vdbeaux.c
// sqlite3VdbeCommit).
func readSuperJournalNames(superPath string) ([]string, error) {
	data, err := os.ReadFile(superPath)
	if err != nil {
		return nil, err
	}
	var names []string
	start := 0
	for i, b := range data {
		if b == 0 {
			if i > start {
				names = append(names, string(data[start:i]))
			}
			start = i + 1
		}
	}
	return names, nil
}

// DeleteSuperJournalIfUnreferenced applies pager_delsuper: the super-journal
// is deleted only when no EXISTING journal still points at it — the last
// participating journal's playback removes it (pager1-4.6.9..4.6.15's
// per-database deletion cascade). superPath names the super-journal; selfJrnl
// is the journal of the pager whose playback just finished (already unlinked,
// so the walk never sees it as a live reference).
func DeleteSuperJournalIfUnreferenced(superPath string) {
	names, err := readSuperJournalNames(superPath)
	if err != nil {
		// The super-journal vanished under the playback: nothing to gate.
		_ = os.Remove(superPath)
		return
	}
	for _, jpath := range names {
		if _, statErr := os.Stat(jpath); statErr != nil {
			continue // this journal is gone: no longer a reference
		}
		data, rerr := os.ReadFile(jpath)
		if rerr != nil {
			return // unreadable reference: keep the super-journal
		}
		if name, ok := SuperJournalPointerFromData(data); ok && name == superPath {
			return // a live journal still points here: keep it
		}
	}
	// No existing journal references the super-journal: the commit point.
	_ = os.Remove(superPath)
}

// FireJournalFileOpEvent exposes the journal-sidecar VFS event hook so the
// multi-file commit can route the super-journal's xDelete through the same
// fixture VFS the journals use (vdbeaux.c's sqlite3OsDelete(pVfs, zSuper, 1)
// — pager1-4.4.x's copy_on_mj_delete and 4.6.x's ::mj_filename capture).
func (p *Pager) FireJournalFileOpEvent(op, path string) {
	if h := p.journalFileOpHookFn(); h != nil {
		h(op, path)
	}
}

// SuperJournalGate reports whether this pager's journal mode participates in
// a super-journal commit (vdbeaux.c's aMJNeeded matrix: DELETE/PERSIST/
// TRUNCATE carry a super-journal; OFF/MEMORY/WAL do not). The empty mode is
// the rollback-journal default (DELETE — sqlite3PagerOpen's
// SQLITE_DEFAULT_JOURNALMODE) and participates; a pager that never saw an
// explicit PRAGMA journal_mode after its ATTACH still journals in DELETE
// mode (journalFileMode maps "" to "delete").
func (p *Pager) SuperJournalGate() bool {
	switch p.journalMode {
	case "", "delete", "persist", "truncate":
		return true
	}
	return false
}

// Path returns the main database file path of this pager ("" for in-memory
// pagers). Exported for the engine's super-journal name selection.
func (p *Pager) Path() string {
	return p.path
}

// HasJournalled reports whether the pager opened a rollback journal for the
// transaction just committed (a journal file sidecar exists or the epoch is
// live): the multi-file commit's phase two finalizes only participating
// pagers.
func (p *Pager) HasJournalled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.journalFile != nil || p.journalAppendOff > 0
}
