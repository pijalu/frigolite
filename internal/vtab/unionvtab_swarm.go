package vtab

import (
	"fmt"
	"strconv"
	"strings"
)

// configureSwarm ports unionConfigureVtab (unionvtab.c): every argument
// following the source statement configures the swarm instance —
// ':name=value' binds a text value to the source statement's SQL parameter,
// maxopen=N sets the open-file limit, missing=/openclose= name the
// not-found/open-close UDFs (prepared in C as SELECT "udf"(...)). Parsing
// happens BEFORE the source statement is stepped so bindings apply first.
func (m *UnionVtabModule) configureSwarm(args []string, cfg *UnionSwarmConfig) error {
	for i, a := range args {
		zArg := unquoteVtabArg(a)
		opt, val, hasVal := splitSwarmOption(zArg)
		if !hasVal {
			// A bare (non "x=y") argument names the missing-row UDF when it
			// is the ONLY aux option; any other bare argument is a parse
			// error reported with the VERBATIM argument text.
			if i == 0 && len(args) == 1 {
				if !m.src.UnionFunctionExists(zArg) {
					return fmt.Errorf("sql error: no such function: %s", zArg)
				}
				cfg.NotFound = zArg
				continue
			}
			return fmt.Errorf("swarmvtab: parse error: %s", a)
		}
		if err := m.applySwarmOption(opt, val, cfg); err != nil {
			return err
		}
	}
	return nil
}

// applySwarmOption applies one "name=value" swarmvtab option.
func (m *UnionVtabModule) applySwarmOption(opt, val string, cfg *UnionSwarmConfig) error {
	switch {
	case strings.HasPrefix(opt, ":"):
		// Bound into the source statement; an unknown parameter is reported
		// when the statement is bound (unionConfigureVtab's
		// sqlite3_bind_parameter_index check).
		cfg.Params = append(cfg.Params, UnionSwarmParam{Name: opt, Value: val})
	case len(opt) == 7 && strings.EqualFold(opt, "maxopen"):
		// C atoi(): a non-numeric value parses as 0, which is illegal.
		if n := atoiPrefix(val); n <= 0 {
			return fmt.Errorf("swarmvtab: illegal maxopen value")
		} else {
			cfg.MaxOpen = n
		}
	case len(opt) == 7 && strings.EqualFold(opt, "missing"):
		if cfg.NotFound != "" {
			return fmt.Errorf("swarmvtab: duplicate \"missing\" option")
		}
		if !m.src.UnionFunctionExists(val) {
			return fmt.Errorf("sql error: no such function: %s", val)
		}
		cfg.NotFound = val
	case len(opt) == 9 && strings.EqualFold(opt, "openclose"):
		if cfg.OpenClose != "" {
			return fmt.Errorf("swarmvtab: duplicate \"openclose\" option")
		}
		if !m.src.UnionFunctionExists(val) {
			return fmt.Errorf("sql error: no such function: %s", val)
		}
		cfg.OpenClose = val
	default:
		return fmt.Errorf("swarmvtab: unrecognized option: %s", opt)
	}
	return nil
}

// splitSwarmOption splits one swarmvtab aux argument into its option name
// (with any leading ':') and value, mirroring the unionConfigureVtab
// tokenizer: optional whitespace, an optional ':' plus id-char run
// (union_isidchar), optional whitespace, then '='. ok is false when no '='
// follows the name.
func splitSwarmOption(z string) (opt, val string, ok bool) {
	i := 0
	for i < len(z) && swarmIsSpace(z[i]) {
		i++
	}
	start := i
	if i < len(z) && z[i] == ':' {
		i++
	}
	for i < len(z) && swarmIsIdChar(z[i]) {
		i++
	}
	opt = z[start:i]
	for i < len(z) && swarmIsSpace(z[i]) {
		i++
	}
	if i >= len(z) || z[i] != '=' {
		return opt, "", false
	}
	i++
	for i < len(z) && swarmIsSpace(z[i]) {
		i++
	}
	return opt, unquoteVtabArg(z[i:]), true
}

// swarmIsSpace mirrors union_isspace (space, tab, CR, LF).
func swarmIsSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t'
}

// swarmIsIdChar mirrors union_isidchar — INCLUDING its upper-bound quirk
// ('A'..'Y', not 'Z') — so option parsing accepts exactly the C character
// set.
func swarmIsIdChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c < 'Z') || (c >= '0' && c <= '9')
}

// atoiPrefix mirrors C atoi(): parse the leading decimal digits (after
// optional whitespace and sign); 0 when there is no numeric prefix.
func atoiPrefix(s string) int {
	i := 0
	for i < len(s) && swarmIsSpace(s[i]) {
		i++
	}
	sign := 1
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		if s[i] == '-' {
			sign = -1
		}
		i++
	}
	j := i
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == i {
		return 0
	}
	n, err := strconv.Atoi(s[i:j])
	if err != nil {
		return 0
	}
	return sign * n
}
