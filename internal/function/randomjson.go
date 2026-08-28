package function

// Faithful Go port of SQLite ext/misc/randomjson.c (2023-04-28): the
// random_json(SEED) and random_json5(SEED) test functions. Given a numeric
// seed they generate deterministic pseudo-random JSON / JSON5 text. The PRNG,
// atom/template tables and expansion algorithm mirror the C source so seeds
// produce byte-identical output to the upstream extension.

import (
	"fmt"
	"math"
	"strings"
)

type jsonPrng struct {
	x, y uint32
}

func (p *jsonPrng) seed(iSeed uint32) {
	p.x = iSeed | 1
	p.y = iSeed
}

func (p *jsonPrng) int() uint32 {
	p.x = (p.x >> 1) ^ ((1 + ^(p.x & 1)) & 0xd0000001)
	p.y = p.y*1103515245 + 12345
	return p.x ^ p.y
}

var azJsonAtoms = [][2]string{
	/* JSON */ /* JSON-5 */
	{"0", "0"},
	{"1", "1"},
	{"-1", "-1"},
	{"2", "+2"},
	{"3DDDD", "3DDDD"},
	{"2.5DD", "2.5DD"},
	{"0.75", ".75"},
	{"-4.0e2", "-4.e2"},
	{"5.0e-3", "+5e-3"},
	{"6.DDe+0DD", "6.DDe+0DD"},
	{"0", "0x0"},
	{"512", "0x200"},
	{"256", "+0x100"},
	{"-2748", "-0xabc"},
	{"true", "true"},
	{"false", "false"},
	{"null", "null"},
	{"9.0e999", "Infinity"},
	{"-9.0e999", "-Infinity"},
	{"9.0e999", "+Infinity"},
	{"null", "NaN"},
	{"-0.0005DD", "-0.0005DD"},
	{"4.35e-3", "+4.35e-3"},
	{"\"gem\\\"hay\"", "\"gem\\\"hay\""},
	{"\"icy'joy\"", "'icy\\'joy'"},
	{"\"keylog\"", "\"key\\\nlog\""},
	{"\"mix\\\\\\tnet\"", "\"mix\\\\\\tnet\""},
	{"\"oat\\r\\n\"", "\"oat\\r\\n\""},
	{"\"\\fpan\\b\"", "\"\\fpan\\b\""},
	{"{}", "{}"},
	{"[]", "[]"},
	{"[]", "[/*empty*/]"},
	{"{}", "{//empty\n}"},
	{"\"ask\"", "\"ask\""},
	{"\"bag\"", "\"bag\""},
	{"\"can\"", "\"can\""},
	{"\"day\"", "\"day\""},
	{"\"end\"", "'end'"},
	{"\"fly\"", "\"fly\""},
	{"\"\\u00XX\\u00XX\"", "\"\\xXX\\xXX\""},
	{"\"y\\uXXXXz\"", "\"y\\uXXXXz\""},
	{"\"\"", "\"\""},
}

var azJsonTemplate = [][2]string{
	{"{\"a\":%,\"b\":%,\"cDD\":%}", "{a:%,b:%,cDD:%}"},
	{"{\"a\":%,\"b\":%,\"c\":%,\"d\":%,\"e\":%}", "{a:%,b:%,c:%,d:%,e:%}"},
	{"{\"a\":%,\"b\":%,\"c\":%,\"d\":%,\"\":%}", "{a:%,b:%,c:%,d:%,'':%}"},
	{"{\"d\":%}", "{d:%}"},
	{"{\"eeee\":%, \"ffff\":%}", "{eeee:% /*and*/, ffff:%}"},
	{"{\"$g\":%,\"_h_\":%,\"a b c d\":%}", "{$g:%,_h_:%,\"a b c d\":%}"},
	{"{\"x\":%,\n  \"y\":%}", "{\"x\":%,\n  \"y\":%}"},
	{"{\"\\u00XX\":%,\"\\uXXXX\":%}", "{\"\\xXX\":%,\"\\uXXXX\":%}"},
	{"{\"Z\":%}", "{Z:%,}"},
	{"[%]", "[%,]"},
	{"[%,%]", "[%,%]"},
	{"[%,%,%]", "[%,%,%,]"},
	{"[%,%,%,%]", "[%,%,%,%]"},
	{"[%,%,%,%,%]", "[%,%,%,%,%]"},
}

const strsz = 10000

const hexDigits = "0123456789abcdef"

// jsonExpand ports randomjson.c jsonExpand: substitute every '%' in zSrc
// with either an atom or a nested template (per the growth probability),
// filling XX/DD placeholders from the PRNG.
func jsonExpand(zSrc string, p *jsonPrng, eType int, r uint32) string {
	var zDest [strsz]byte
	j := 0
	if zSrc == "" {
		zSrc = "%"
	}
	if len(zSrc) >= strsz/10 {
		r = 0
	}
	for i := 0; i < len(zSrc); i++ {
		if zSrc[i] != '%' {
			if j < strsz {
				zDest[j] = zSrc[i]
				j++
			}
			continue
		}
		var z string
		if r == 0 || (r < 1000 && uint32(p.int()%1000) <= r) {
			// azJsonAtoms holds one row per atom pair ([0]=JSON, [1]=JSON5),
			// so the draw indexes rows directly (C computes k over
			// count(flat)/2 which equals the number of pairs).
			k := int(p.int() % uint32(len(azJsonAtoms)))
			z = azJsonAtoms[k][eType]
		} else {
			k := int(p.int() % uint32(len(azJsonTemplate)))
			z = azJsonTemplate[k][eType]
		}
		if strings.Contains(z, "XX") {
			y := p.int()
			if y&0xff == (y>>8)&0xff {
				y += 0x100
			}
			for (y&0xff == (y>>16)&0xff) || ((y>>8)&0xff == (y>>16)&0xff) {
				y += 0x10000
			}
			var sb strings.Builder
			for pos := 0; pos < len(z); pos++ {
				if pos+1 < len(z) && z[pos] == 'X' && z[pos+1] == 'X' {
					sb.WriteByte(hexDigits[y%16])
					y /= 16
					sb.WriteByte(hexDigits[y%16])
					y /= 16
					pos++
					continue
				}
				sb.WriteByte(z[pos])
			}
			z = sb.String()
		} else if strings.Contains(z, "DD") {
			y := p.int()
			var sb strings.Builder
			for pos := 0; pos < len(z); pos++ {
				if pos+1 < len(z) && z[pos] == 'D' && z[pos+1] == 'D' {
					sb.WriteByte("0123456789"[y%10])
					y /= 10
					sb.WriteByte("0123456789"[y%10])
					y /= 10
					pos++
					continue
				}
				sb.WriteByte(z[pos])
			}
			z = sb.String()
		}
		if j+len(z) < strsz {
			copy(zDest[j:], z)
			j += len(z)
		}
	}
	return string(zDest[:j])
}

// randomJSONFunc ports randomjson.c randJsonFunc: four expansion passes
// (grow, grow, prune-to-100, atoms-only) produce the final text.
func randomJSONFunc(seed int64, json5 bool) string {
	eType := 0
	if json5 {
		eType = 1
	}
	var p jsonPrng
	p.seed(uint32(seed))
	z2 := jsonExpand("", &p, eType, 1000)
	z1 := jsonExpand(z2, &p, eType, 1000)
	z2 = jsonExpand(z1, &p, eType, 100)
	z1 = jsonExpand(z2, &p, eType, 0)
	return z1
}

// fnRANDOM_JSON implements random_json(SEED)/random_json5(SEED) for the
// json106/json108 test suites (load_static_extension randomjson equivalent).
func fnRANDOM_JSON(json5 bool) func([]interface{}) (interface{}, error) {
	return func(args []interface{}) (interface{}, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("random_json() expects exactly one argument")
		}
		f, _ := numericArg(args[0])
		f = math.Trunc(f)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, nil
		}
		seed := int64(f)
		if f < 0 && f > math.MinInt32 {
			// C sqlite3_value_int truncates to 32 bits via cast semantics.
			seed = int64(int32(f))
		}
		return randomJSONFunc(seed, json5), nil
	}
}
