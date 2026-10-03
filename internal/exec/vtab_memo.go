package exec

import "strings"

// notUpdaterVtabMemo reports whether name is memoized as a non-vtab DML
// target (see VTabUpdaterInstance).
func (e *Engine) notUpdaterVtabMemo(name string) bool {
	fp := e.allSchemasFingerprint()
	if e.notUpdaterVtabFP != fp {
		return false
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
	}
	if e.notUpdaterVtabNames == nil {
		e.notUpdaterVtabNames = make(map[string]struct{})
	}
	e.notUpdaterVtabNames[strings.ToLower(name)] = struct{}{}
}
