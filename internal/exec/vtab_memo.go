package exec

import "strings"

// notUpdaterVtabMemo reports whether name is memoized as a non-vtab DML
// target (see VTabUpdaterInstance). The single-slot last entry answers the
// OLTP shape (the same table named statement after statement) without the
// per-statement map hash.
func (e *Engine) notUpdaterVtabMemo(name string) bool {
	fp := e.allSchemasFingerprint()
	if e.notUpdaterVtabFP != fp {
		return false
	}
	if e.notUpdaterLastName == name {
		return e.notUpdaterLastOK
	}
	_, ok := e.notUpdaterVtabNames[strings.ToLower(name)]
	return ok
}

// rememberNotUpdaterVtab records name as a non-vtab DML target under the
// current all-schemas fingerprint (see VTabUpdaterInstance).
func (e *Engine) rememberNotUpdaterVtab(name string) {
	fp := e.allSchemasFingerprint()
	if e.notUpdaterVtabFP != fp {
		e.notUpdaterVtabFP = fp
		e.notUpdaterVtabNames = nil
		e.notUpdaterLastName = ""
	}
	e.notUpdaterLastName = name
	e.notUpdaterLastOK = true
	if e.notUpdaterVtabNames == nil {
		e.notUpdaterVtabNames = make(map[string]struct{})
	}
	e.notUpdaterVtabNames[strings.ToLower(name)] = struct{}{}
}
