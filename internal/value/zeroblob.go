package value

// ZeroBlob is the lazy representation of SQLite's zeroblob(N): a BLOB of N
// zero bytes whose content is not materialized until needed. SQLite keeps
// this as an MEM_Blob with MEM_Zero (n=0, u.nZero=N) and expands it only
// when content is required (record encoding of a mid-record column,
// comparisons, string ops). See src/vdbe.c updateMaxBlobsize/OP_MakeRecord
// and src/vdbemem.c sqlite3VdbeMemExpandBlob.
//
// The distinction matters for the test-only sqlite3_max_blobsize global: a
// trailing zeroblob in a record is counted as zero content bytes (the record
// is stored with a "nZero bytes of zero tail" instead of a materialized
// buffer), while a genuinely zero-filled blob literal counts all N bytes.
type ZeroBlob struct{ N int }

// Bytes expands the zero blob into a materialized byte slice, matching
// sqlite3VdbeMemExpandBlob.
func (z ZeroBlob) Bytes() []byte { return make([]byte, z.N) }
