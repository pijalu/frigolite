// JSON1 aggregates: json_array_length and the json_group_array /
// json_group_object aggregate functions.

package function

import (
	"fmt"
)

// fnJSON_ARRAY_LENGTH implements json_array_length(X[,P]): the number of
// elements of the array at P (or of X); a non-array value counts as 0.
func fnJSON_ARRAY_LENGTH(args []interface{}) (interface{}, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	root, err := parseJSONArg(args[0])
	if err != nil {
		return nil, err
	}
	node := root
	if len(args) > 1 {
		// sqlite jsonArrayLengthFunc: a NULL path yields SQL NULL.
		if args[1] == nil {
			return nil, nil
		}
		comps, perr := parseJSONPath(toString(args[1]))
		if perr != nil {
			return nil, perr
		}
		found, ok := jsonLookup(root, comps)
		if !ok {
			return nil, nil
		}
		node = found
	}
	if node.kind != jsonArray {
		return int64(0), nil
	}
	return int64(len(node.arr)), nil
}

// jsonGroupArrayAgg implements json_group_array(V): a JSON array of every
// stepped value.
type jsonGroupArrayAgg struct {
	items []*jsonNode
}

func (a *jsonGroupArrayAgg) Step(args []interface{}) error {
	v, err := jsonInsertValue(args[0])
	if err != nil {
		return err
	}
	a.items = append(a.items, v)
	return nil
}

func (a *jsonGroupArrayAgg) Final() (interface{}, error) {
	n := &jsonNode{kind: jsonArray, arr: a.items}
	return JSONText(jsonSerialize(n)), nil
}

// jsonGroupObjectAgg implements json_group_object(K,V): a JSON object built
// from key/value argument pairs.
type jsonGroupObjectAgg struct {
	pairs []jsonPair
}

func (a *jsonGroupObjectAgg) Step(args []interface{}) error {
	if len(args) < 2 {
		return fmt.Errorf("json_group_object() needs two arguments")
	}
	// json.c jsonGroupObject: a NULL field name omits the pair entirely
	// (json101-21.29: (NULL,'three') contributes nothing to the object).
	if args[0] == nil {
		return nil
	}
	key := fmt.Sprint(args[0])
	v, err := jsonInsertValue(args[1])
	if err != nil {
		return err
	}
	a.pairs = append(a.pairs, jsonPair{key: key, value: v})
	return nil
}

func (a *jsonGroupObjectAgg) Final() (interface{}, error) {
	n := &jsonNode{kind: jsonObject, obj: a.pairs}
	return JSONText(jsonSerialize(n)), nil
}
