package vtab

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// spellfix extension SQL functions (spellfix.c spellfix1InstallFunc +
// editDist3Install + ext/misc/nextchar.c): editdist, spellfix1_translit,
// spellfix1_editdist, spellfix1_phonehash, spellfix1_scriptcode,
// editdist3, next_char.

// RegisterSpellfixSQLFunctions registers the spellfix extension's global SQL
// functions on the connection. The editdist3 configuration is shared by the
// closures created here (editDist3Install's pConfig).
func RegisterSpellfixSQLFunctions(db Database) {
	db.RegisterScalar("editdist", 2, 2, func(args []interface{}) (interface{}, error) {
		a, _ := args[0].(string)
		b, _ := args[1].(string)
		res := editdist1(a, b, nil)
		switch {
		case res == -2:
			return nil, fmt.Errorf("non-ASCII input to editdist()")
		case res < 0:
			return nil, fmt.Errorf("NULL input to editdist()")
		}
		return int64(res), nil
	})
	db.RegisterScalar("spellfix1_translit", 1, 1, func(args []interface{}) (interface{}, error) {
		s, _ := args[0].(string)
		return transliterate(s), nil
	})
	db.RegisterScalar("spellfix1_editdist", 2, 2, func(args []interface{}) (interface{}, error) {
		a, _ := args[0].(string)
		b, _ := args[1].(string)
		return int64(editdist1(a, b, nil)), nil
	})
	db.RegisterScalar("spellfix1_phonehash", 1, 1, func(args []interface{}) (interface{}, error) {
		s, _ := args[0].(string)
		return phoneticHash(s), nil
	})
	db.RegisterScalar("spellfix1_scriptcode", 1, 1, func(args []interface{}) (interface{}, error) {
		s, _ := args[0].(string)
		return int64(spellfix1ScriptCode(s)), nil
	})

	// editdist3: the 1-arg form loads a cost table into the shared config;
	// the 2/3-arg forms measure the distance (editDist3SqlFunc).
	cfg := &spellfixEditDist3Config{}
	db.RegisterScalar("editdist3", 1, 3, func(args []interface{}) (interface{}, error) {
		if len(args) == 1 {
			table, _ := args[0].(string)
			loaded, err := spellfixEditDist3ConfigLoad(db, table)
			if err != nil {
				return nil, err
			}
			*cfg = *loaded
			return nil, nil
		}
		a, _ := args[0].(string)
		b, _ := args[1].(string)
		iLang := 0
		if len(args) == 3 {
			iLang = spellfixValueInt(args[2])
		}
		// SQLITE_TOOBIG parity (spellfix4 400/401/410): inputs beyond the
		// 10000-byte statement-string limit fail with
		// "string or blob too big"; exactly 10000 still computes.
		if len(a) > 10000 || len(b) > 10000 {
			return nil, fmt.Errorf("string or blob too big")
		}
		lang := spellfixEditDist3FindLang(cfg, iLang)
		from := spellfixFromStringNew(lang, a)
		return int64(spellfixEditDist3Core(from, b, lang, nil)), nil
	})

	// next_char(A,T,F[,W][,C]) (ext/misc/nextchar.c): all distinct next
	// characters continuing prefix A within T.F.
	db.RegisterScalar("next_char", 3, 5, func(args []interface{}) (interface{}, error) {
		return spellfixNextChar(db, args)
	})
}

// spellfixNextChar ports nextCharFunc/findNextChars: repeatedly select the
// smallest T.F value that starts with the prefix and sorts after the last
// found value, harvesting the character that follows the prefix.
func spellfixNextChar(db Database, args []interface{}) (interface{}, error) {
	prefix, _ := args[0].(string)
	table, _ := args[1].(string)
	field, _ := args[2].(string)
	if args[0] == nil || args[1] == nil || args[2] == nil {
		return nil, nil
	}
	sqlStr := spellfixNextCharSQL(table, field, spellfixNextCharArg(args, 3), spellfixNextCharArg(args, 4))

	seen := map[rune]bool{}
	var out []byte
	cPrev := rune(0)
	for {
		// ?1 = prefix, ?2 = utf8(cPrev+1) (findNextChars loop).
		q := strings.ReplaceAll(sqlStr, "?1", spellfixQuote(prefix))
		q = strings.ReplaceAll(q, "?2", spellfixQuote(string(cPrev+1)))
		rows, err := db.ExecSQL(q)
		if err != nil {
			return nil, err
		}
		val, done := spellfixNextCharRow(rows, prefix)
		if done {
			return string(out), nil // SQLITE_DONE: no further candidates
		}
		r, size := utf8.DecodeRuneInString(val[len(prefix):])
		if size == 0 {
			return string(out), nil
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, string(r)...)
		}
		cPrev = r
	}
}

// spellfixNextCharArg reads optional string args[4] (WHERE conjunct) and
// args[5] (collation name).
func spellfixNextCharArg(args []interface{}, idx int) string {
	if len(args) > idx {
		if s, ok := args[idx].(string); ok {
			return s
		}
	}
	return ""
}

// spellfixNextCharSQL builds the findNextChars probe statement.
func spellfixNextCharSQL(table, field, where, collation string) string {
	whereClause := ""
	if where != "" {
		whereClause = "AND (" + where + ")"
	}
	coll := ""
	if collation != "" {
		coll = `collate "` + collation + `"`
	}
	return fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s>=(?1 || ?2) %s AND %s<=(?1 || char(1114111)) %s %s ORDER BY 1 %s ASC LIMIT 1",
		field, table, field, coll, field, coll, whereClause, coll)
}

// spellfixNextCharRow evaluates one probe result; done reports the
// SQLITE_DONE condition (empty row/short value).
func spellfixNextCharRow(rows [][]interface{}, prefix string) (val string, done bool) {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return "", true
	}
	val, _ = rows[0][0].(string)
	if len(val) < len(prefix) {
		return "", true
	}
	return val, false
}
