package sql

import "strings"

// parseIndexColumnCollations extracts per-column COLLATE names from a CREATE
// INDEX column list ("CREATE INDEX i ON t(a, b COLLATE rtrim)" -> ["", "rtrim"]).
func ParseIndexColumnCollations(sqlStr string) []string {
	upper := strings.ToUpper(sqlStr)
	start := strings.Index(upper, "(")
	if start < 0 {
		return nil
	}
	end := strings.LastIndex(upper, ")")
	if end < 0 || end <= start {
		return nil
	}
	colsStr := sqlStr[start+1 : end]
	var colls []string
	for _, c := range strings.Split(colsStr, ",") {
		col := strings.TrimSpace(c)
		cu := strings.ToUpper(col)
		if idx := strings.Index(cu, " COLLATE "); idx >= 0 {
			rest := strings.TrimSpace(col[idx+len(" COLLATE "):])
			// Strip trailing ASC/DESC.
			ru := strings.ToUpper(rest)
			if di := strings.Index(ru, " DESC"); di >= 0 {
				rest = strings.TrimSpace(rest[:di])
			} else if ai := strings.Index(ru, " ASC"); ai >= 0 {
				rest = strings.TrimSpace(rest[:ai])
			}
			colls = append(colls, rest)
		} else {
			colls = append(colls, "")
		}
	}
	return colls
}
