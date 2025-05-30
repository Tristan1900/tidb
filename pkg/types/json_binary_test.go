// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package types

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

const jsonBenchStr = `{"a":[1,"2",{"aa":"bb"},4,null],"b":true,"c":null}`

func TestBinaryJSONMarshalUnmarshal(t *testing.T) {
	expectedList := []string{
		`{"a": [1, "2", {"aa": "bb"}, 4, null], "b": true, "c": null}`,
		`{"aaaaaaaaaaa": [1, "2", {"aa": "bb"}, 4.1], "bbbbbbbbbb": true, "ccccccccc": "d"}`,
		`[{"a": 1, "b": true}, 3, 3.5, "hello, world", null, true]`,
		`{"a": "&<>"}`,
	}

	for _, expected := range expectedList {
		result := mustParseBinaryFromString(t, expected)
		require.Equal(t, expected, result.String())
	}
}

func TestBinaryJSONExtract(t *testing.T) {
	bj1 := mustParseBinaryFromString(t, `{"\"hello\"": "world", "a": [1, "2", {"aa": "bb"}, 4.0, {"aa": "cc"}], "b": true, "c": ["d"]}`)
	bj2 := mustParseBinaryFromString(t, `[{"a": 1, "b": true}, 3, 3.5, "hello, world", null, true]`)
	bj3 := mustParseBinaryFromString(t, `{"properties": {"$type": "TiDB"}}`)
	bj4 := mustParseBinaryFromString(t, `{"properties": {"$type$type": {"$a$a" : "TiDB"}}}`)
	bj5 := mustParseBinaryFromString(t, `{"properties": {"$type": {"$a" : {"$b" : "TiDB"}}}}`)
	bj6 := mustParseBinaryFromString(t, `{"properties": {"$type": {"$a$a" : "TiDB"}},"hello": {"$b$b": "world","$c": "amazing"}}`)
	bj7 := mustParseBinaryFromString(t, `{ "a": { "x" : { "b": { "y": { "b": { "z": { "c": 100 } } } } } } }`)
	bj8 := mustParseBinaryFromString(t, `{ "a": { "b" : [ 1, 2, 3 ] } }`)
	bj9 := mustParseBinaryFromString(t, `[[0,1],[2,3],[4,[5,6]]]`)
	bj10 := mustParseBinaryFromString(t, `[1]`)
	bj11 := mustParseBinaryFromString(t, `{"metadata": {"comment": "1234"}}`)
	bj12 := mustParseBinaryFromString(t, `{"metadata": {"age": 19, "name": "Tom"}}`)

	var tests = []struct {
		bj              BinaryJSON
		pathExprStrings []string
		expected        BinaryJSON
		found           bool
		err             error
	}{
		// test extract with only one path expression.
		{bj1, []string{"$.a"}, mustParseBinaryFromString(t, `[1, "2", {"aa": "bb"}, 4.0, {"aa": "cc"}]`), true, nil},
		{bj2, []string{"$.a"}, mustParseBinaryFromString(t, "null"), false, nil},
		{bj1, []string{"$[0]"}, bj1, true, nil}, // in Extract, autowraped bj1 as an array.
		{bj2, []string{"$[0]"}, mustParseBinaryFromString(t, `{"a": 1, "b": true}`), true, nil},
		{bj1, []string{"$.a[2].aa"}, mustParseBinaryFromString(t, `"bb"`), true, nil},
		{bj1, []string{"$.a[*].aa"}, mustParseBinaryFromString(t, `["bb", "cc"]`), true, nil},
		{bj1, []string{"$.*[0]"}, mustParseBinaryFromString(t, `["world", 1, true, "d"]`), true, nil},
		{bj1, []string{`$.a[*]."aa"`}, mustParseBinaryFromString(t, `["bb", "cc"]`), true, nil},
		{bj1, []string{`$."\"hello\""`}, mustParseBinaryFromString(t, `"world"`), true, nil},
		{bj1, []string{`$**[1]`}, mustParseBinaryFromString(t, `["2"]`), true, nil},
		{bj3, []string{`$.properties.$type`}, mustParseBinaryFromString(t, `"TiDB"`), true, nil},
		{bj4, []string{`$.properties.$type$type`}, mustParseBinaryFromString(t, `{"$a$a" : "TiDB"}`), true, nil},
		{bj4, []string{`$.properties.$type$type.$a$a`}, mustParseBinaryFromString(t, `"TiDB"`), true, nil},
		{bj5, []string{`$.properties.$type.$a.$b`}, mustParseBinaryFromString(t, `"TiDB"`), true, nil},
		{bj5, []string{`$.properties.$type.$a.*[0]`}, mustParseBinaryFromString(t, `["TiDB"]`), true, nil},
		{bj11, []string{"$.metadata.comment"}, mustParseBinaryFromString(t, `"1234"`), true, nil},
		{bj9, []string{"$[0]"}, mustParseBinaryFromString(t, `[0, 1] `), true, nil},
		{bj9, []string{"$[last][last]"}, mustParseBinaryFromString(t, `[5,6]`), true, nil},
		{bj9, []string{"$[last-1][last]"}, mustParseBinaryFromString(t, `3`), true, nil},
		{bj9, []string{"$[last-1][last-1]"}, mustParseBinaryFromString(t, `2`), true, nil},
		{bj9, []string{"$[1 to 2]"}, mustParseBinaryFromString(t, `[[2,3],[4,[5,6]]]`), true, nil},
		{bj9, []string{"$[1 to 2][1 to 2]"}, mustParseBinaryFromString(t, `[3,[5,6]]`), true, nil},
		{bj9, []string{"$[1 to last][1 to last]"}, mustParseBinaryFromString(t, `[3,[5,6]]`), true, nil},
		{bj9, []string{"$[1 to last][1 to last - 1]"}, bj9, false, nil},
		{bj9, []string{"$[1 to last][0 to last - 1]"}, mustParseBinaryFromString(t, `[2,4]`), true, nil},

		// test extract with multi path expressions.
		{bj1, []string{"$.a", "$[5]"}, mustParseBinaryFromString(t, `[[1, "2", {"aa": "bb"}, 4.0, {"aa": "cc"}]]`), true, nil},
		{bj2, []string{"$.a", "$[0]"}, mustParseBinaryFromString(t, `[{"a": 1, "b": true}]`), true, nil},
		{bj6, []string{"$.properties", "$[1]"}, mustParseBinaryFromString(t, `[{"$type": {"$a$a" : "TiDB"}}]`), true, nil},
		{bj6, []string{"$.hello", "$[2]"}, mustParseBinaryFromString(t, `[{"$b$b": "world","$c": "amazing"}]`), true, nil},
		{bj7, []string{"$.a**.b**.c"}, mustParseBinaryFromString(t, `[100]`), true, nil},
		{bj8, []string{"$**[0]"}, mustParseBinaryFromString(t, `[{"a": {"b": [1, 2, 3]}}, {"b": [1, 2, 3]}, 1, 2, 3]`), true, nil},
		{bj9, []string{"$**[0]"}, mustParseBinaryFromString(t, `[[0, 1], 0, 1, 2, 3, 4, 5, 6] `), true, nil},
		{bj10, []string{"$**[0]"}, mustParseBinaryFromString(t, `[1]`), true, nil},
		{bj12, []string{"$.metadata.age", "$.metadata.name"}, mustParseBinaryFromString(t, `[19, "Tom"]`), true, nil},
	}

	for _, test := range tests {
		var pathExprList = make([]JSONPathExpression, 0)
		for _, peStr := range test.pathExprStrings {
			pe, err := ParseJSONPathExpr(peStr)
			require.NoError(t, err)
			pathExprList = append(pathExprList, pe)
		}

		result, found := test.bj.Extract(pathExprList)
		require.Equal(t, test.found, found, test.bj.String())
		if found {
			require.Equal(t, test.expected.String(), result.String())
		}
	}
}

func TestBinaryJSONType(t *testing.T) {
	var tests = []struct {
		in  string
		out string
	}{
		{`{"a": "b"}`, "OBJECT"},
		{`["a", "b"]`, "ARRAY"},
		{`3`, "INTEGER"},
		{`3.0`, "DOUBLE"},
		{`null`, "NULL"},
		{`true`, "BOOLEAN"},
	}

	for _, test := range tests {
		result := mustParseBinaryFromString(t, test.in)
		require.Equal(t, test.out, result.Type())
	}

	// we can't parse '9223372036854775808' to JSON::Uint64 now,
	// because go builtin JSON parser treats that as DOUBLE.
	require.Equal(t, "UNSIGNED INTEGER", CreateBinaryJSON(uint64(1<<63)).Type())
}

func TestBinaryJSONUnquote(t *testing.T) {
	var tests = []struct {
		json     string
		unquoted string
	}{
		{json: `3`, unquoted: "3"},
		{json: `"3"`, unquoted: "3"},
		{json: `"[{\"x\":\"{\\\"y\\\":12}\"}]"`, unquoted: `[{"x":"{\"y\":12}"}]`},
		{json: `"hello, \"escaped quotes\" world"`, unquoted: "hello, \"escaped quotes\" world"},
		{json: "\"\\u4f60\"", unquoted: "你"},
		{json: `true`, unquoted: "true"},
		{json: `null`, unquoted: "null"},
		{json: `{"a": [1, 2]}`, unquoted: `{"a": [1, 2]}`},
		{json: `"'"`, unquoted: `'`},
		{json: `"''"`, unquoted: `''`},
		{json: `""`, unquoted: ``},
	}

	for _, test := range tests {
		result := mustParseBinaryFromString(t, test.json)
		unquoted, err := result.Unquote()
		require.NoError(t, err)
		require.Equal(t, test.unquoted, unquoted)
	}
}

func TestQuoteString(t *testing.T) {
	var tests = []struct {
		raw    string
		quoted string
	}{
		{raw: "3", quoted: `"3"`},
		{raw: "hello, \"escaped quotes\" world", quoted: `"hello, \"escaped quotes\" world"`},
		{raw: "你", quoted: `"你"`},
		{raw: "true", quoted: `true`},
		{raw: "null", quoted: `null`},
		{raw: `"`, quoted: `"\""`},
		{raw: `'`, quoted: `"'"`},
		{raw: `''`, quoted: `"''"`},
		{raw: ``, quoted: `""`},
		{raw: "\\ \" \b \f \n \r \t", quoted: `"\\ \" \b \f \n \r \t"`},
	}

	for _, test := range tests {
		require.Equal(t, test.quoted, quoteJSONString(test.raw))
	}
}

func TestBinaryJSONModify(t *testing.T) {
	var tests = []struct {
		base     string
		setField string
		setValue string
		expected string
		success  bool
		mt       JSONModifyType
	}{
		{`null`, "$", `{}`, `{}`, true, JSONModifySet},
		{`{}`, "$.a", `3`, `{"a": 3}`, true, JSONModifySet},
		{`{"a": 3}`, "$.a", `[]`, `{"a": []}`, true, JSONModifyReplace},
		{`{"a": 3}`, "$.b", `"3"`, `{"a": 3, "b": "3"}`, true, JSONModifySet},
		{`{"a": []}`, "$.a[0]", `3`, `{"a": [3]}`, true, JSONModifySet},
		{`{"a": [3]}`, "$.a[1]", `4`, `{"a": [3, 4]}`, true, JSONModifyInsert},
		{`{"a": [3]}`, "$[0]", `4`, `4`, true, JSONModifySet},
		{`{"a": [3]}`, "$[1]", `4`, `[{"a": [3]}, 4]`, true, JSONModifySet},
		{`{"b": true}`, "$.b", `false`, `{"b": false}`, true, JSONModifySet},

		// These tests illustrate the differences among the three JSONModifyType
		{`{"foo": "bar"}`, "$.foo", `"moo"`, `{"foo": "bar"}`, true, JSONModifyInsert},
		{`{"foo": "bar"}`, "$.foo", `"moo"`, `{"foo": "moo"}`, true, JSONModifyReplace},
		{`{"foo": "bar"}`, "$.foo", `"moo"`, `{"foo": "moo"}`, true, JSONModifySet},
		{`{"foo": "bar"}`, "$.foo", `null`, `{"foo": null}`, true, JSONModifySet},
		{`{"foo": "bar"}`, "$.baz", `"moo"`, `{"foo": "bar", "baz": "moo"}`, true, JSONModifyInsert},
		{`{"foo": "bar"}`, "$.baz", `"moo"`, `{"foo": "bar"}`, true, JSONModifyReplace},
		{`{"foo": "bar"}`, "$.baz", `"moo"`, `{"foo": "bar", "baz": "moo"}`, true, JSONModifySet},
		{`{"foo": "bar"}`, "$.baz", `null`, `{"foo": "bar", "baz": null}`, true, JSONModifySet},

		// nothing changed because the path is empty and we want to insert.
		{`{}`, "$", `1`, `{}`, true, JSONModifyInsert},
		// nothing changed because the path without last leg doesn't exist.
		{`{"a": [3, 4]}`, "$.b[1]", `3`, `{"a": [3, 4]}`, true, JSONModifySet},
		// nothing changed because the path without last leg doesn't exist.
		{`{"a": [3, 4]}`, "$.a[2].b", `3`, `{"a": [3, 4]}`, true, JSONModifySet},
		// nothing changed because we want to insert but the full path exists.
		{`{"a": [3, 4]}`, "$.a[0]", `30`, `{"a": [3, 4]}`, true, JSONModifyInsert},
		// nothing changed because we want to replace but the full path doesn't exist.
		{`{"a": [3, 4]}`, "$.a[2]", `30`, `{"a": [3, 4]}`, true, JSONModifyReplace},

		// bad path expression.
		{"null", "$.*", "{}", "null", false, JSONModifySet},
		{"null", "$[*]", "{}", "null", false, JSONModifySet},
		{"null", "$**.a", "{}", "null", false, JSONModifySet},
		{"null", "$**[3]", "{}", "null", false, JSONModifySet},
	}

	for _, test := range tests {
		pathExpr, err := ParseJSONPathExpr(test.setField)
		require.NoError(t, err)

		base := mustParseBinaryFromString(t, test.base)
		value := mustParseBinaryFromString(t, test.setValue)
		expected := mustParseBinaryFromString(t, test.expected)
		obtain, err := base.Modify([]JSONPathExpression{pathExpr}, []BinaryJSON{value}, test.mt)
		if test.success {
			require.NoError(t, err)
			require.Equal(t, expected.String(), obtain.String())
		} else {
			require.Error(t, err)
		}
	}
}

func TestBinaryJSONRemove(t *testing.T) {
	var tests = []struct {
		base     string
		path     string
		expected string
		success  bool
	}{
		{`null`, "$", `{}`, false},
		{`{"a":[3]}`, "$.a[*]", `{"a":[3]}`, false},
		{`{}`, "$.a", `{}`, true},
		{`{"a":3}`, "$.a", `{}`, true},
		{`{"a":1,"b":2,"c":3}`, "$.b", `{"a":1,"c":3}`, true},
		{`{"a":1,"b":2,"c":3}`, "$.d", `{"a":1,"b":2,"c":3}`, true},
		{`{"a":3}`, "$[0]", `{"a":3}`, true},
		{`{"a":[3,4,5]}`, "$.a[0]", `{"a":[4,5]}`, true},
		{`{"a":[3,4,5]}`, "$.a[1]", `{"a":[3,5]}`, true},
		{`{"a":[3,4,5]}`, "$.a[4]", `{"a":[3,4,5]}`, true},
		{`{"a": [1, 2, {"aa": "xx"}]}`, "$.a[2].aa", `{"a": [1, 2, {}]}`, true},
	}

	for _, test := range tests {
		pathExpr, err := ParseJSONPathExpr(test.path)
		require.NoError(t, err)

		base := mustParseBinaryFromString(t, test.base)
		expected := mustParseBinaryFromString(t, test.expected)
		obtain, err := base.Remove([]JSONPathExpression{pathExpr})
		if test.success {
			require.NoError(t, err)
			require.Equal(t, expected.String(), obtain.String())
		} else {
			require.Error(t, err)
		}
	}
}

func TestCompareBinary(t *testing.T) {
	jNull := mustParseBinaryFromString(t, `null`)
	jBoolTrue := mustParseBinaryFromString(t, `true`)
	jBoolFalse := mustParseBinaryFromString(t, `false`)
	jIntegerLarge := CreateBinaryJSON(uint64(1 << 63))
	jIntegerSmall := mustParseBinaryFromString(t, `3`)
	jStringLarge := mustParseBinaryFromString(t, `"hello, world"`)
	jStringSmall := mustParseBinaryFromString(t, `"hello"`)
	jArrayLarge := mustParseBinaryFromString(t, `["a", "c"]`)
	jArraySmall := mustParseBinaryFromString(t, `["a", "b"]`)
	jObject := mustParseBinaryFromString(t, `{"a": "b"}`)

	var tests = []struct {
		left   BinaryJSON
		right  BinaryJSON
		result int
	}{
		{jNull, jIntegerSmall, -1},
		{jIntegerSmall, jIntegerLarge, -1},
		{jIntegerLarge, jStringSmall, -1},
		{jStringSmall, jStringLarge, -1},
		{jStringLarge, jObject, -1},
		{jObject, jArraySmall, -1},
		{jArraySmall, jArrayLarge, -1},
		{jArrayLarge, jBoolFalse, -1},
		{jBoolFalse, jBoolTrue, -1},
		{CreateBinaryJSON(int64(922337203685477580)), CreateBinaryJSON(int64(922337203685477580)), 0},
		{CreateBinaryJSON(int64(922337203685477580)), CreateBinaryJSON(int64(922337203685477581)), -1},
		{CreateBinaryJSON(int64(922337203685477581)), CreateBinaryJSON(int64(922337203685477580)), 1},

		{CreateBinaryJSON(int64(-1)), CreateBinaryJSON(uint64(18446744073709551615)), -1},
		{CreateBinaryJSON(int64(922337203685477580)), CreateBinaryJSON(uint64(922337203685477581)), -1},
		{CreateBinaryJSON(int64(2)), CreateBinaryJSON(uint64(1)), 1},
		{CreateBinaryJSON(int64(math.MaxInt64)), CreateBinaryJSON(uint64(math.MaxInt64)), 0},

		{CreateBinaryJSON(uint64(18446744073709551615)), CreateBinaryJSON(int64(-1)), 1},
		{CreateBinaryJSON(uint64(922337203685477581)), CreateBinaryJSON(int64(922337203685477580)), 1},
		{CreateBinaryJSON(uint64(1)), CreateBinaryJSON(int64(2)), -1},
		{CreateBinaryJSON(uint64(math.MaxInt64)), CreateBinaryJSON(int64(math.MaxInt64)), 0},

		{CreateBinaryJSON(float64(9.0)), CreateBinaryJSON(int64(9)), 0},
		{CreateBinaryJSON(float64(8.9)), CreateBinaryJSON(int64(9)), -1},
		{CreateBinaryJSON(float64(9.1)), CreateBinaryJSON(int64(9)), 1},

		{CreateBinaryJSON(float64(9.0)), CreateBinaryJSON(uint64(9)), 0},
		{CreateBinaryJSON(float64(8.9)), CreateBinaryJSON(uint64(9)), -1},
		{CreateBinaryJSON(float64(9.1)), CreateBinaryJSON(uint64(9)), 1},

		{CreateBinaryJSON(int64(9)), CreateBinaryJSON(float64(9.0)), 0},
		{CreateBinaryJSON(int64(9)), CreateBinaryJSON(float64(8.9)), 1},
		{CreateBinaryJSON(int64(9)), CreateBinaryJSON(float64(9.1)), -1},

		{CreateBinaryJSON(uint64(9)), CreateBinaryJSON(float64(9.0)), 0},
		{CreateBinaryJSON(uint64(9)), CreateBinaryJSON(float64(8.9)), 1},
		{CreateBinaryJSON(uint64(9)), CreateBinaryJSON(float64(9.1)), -1},
	}

	for _, test := range tests {
		result := CompareBinaryJSON(test.left, test.right)
		comment := fmt.Sprintf("left: %v, right: %v, expect: %v, got: %v", test.left, test.right, test.result, result)
		require.Equal(t, test.result, result, comment)
	}
}

func TestBinaryJSONMerge(t *testing.T) {
	var tests = []struct {
		suffixes []string
		expected string
	}{
		{[]string{`{"a": 1}`, `{"b": 2}`}, `{"a": 1, "b": 2}`},
		{[]string{`{"a": 1}`, `{"a": 2}`}, `{"a": [1, 2]}`},
		{[]string{`[1]`, `[2]`}, `[1, 2]`},
		{[]string{`{"a": 1}`, `[1]`}, `[{"a": 1}, 1]`},
		{[]string{`[1]`, `{"a": 1}`}, `[1, {"a": 1}]`},
		{[]string{`{"a": 1}`, `4`}, `[{"a": 1}, 4]`},
		{[]string{`[1]`, `4`}, `[1, 4]`},
		{[]string{`4`, `{"a": 1}`}, `[4, {"a": 1}]`},
		{[]string{`4`, `1`}, `[4, 1]`},
		{[]string{`{}`, `[]`}, `[{}]`},
		{[]string{`{"comment": "1234"}`, `{"age": 19, "name": "Tom"}`}, `{"age": 19, "comment": "1234", "name": "Tom"}`},
		{[]string{`{"metadata": {"comment": "1234"}}`, `{"metadata": {"age": 19, "name": "Tom"}}`}, `{"metadata": {"age": 19, "comment": "1234", "name": "Tom"}}`},
		{[]string{`{"comment": "1234"}`, `{"comment": "abc"}`}, `{"comment": ["1234", "abc"]}`},
	}

	for _, test := range tests {
		suffixes := make([]BinaryJSON, 0, len(test.suffixes)+1)
		for _, s := range test.suffixes {
			suffixes = append(suffixes, mustParseBinaryFromString(t, s))
		}
		result := MergeBinaryJSON(suffixes)
		cmp := CompareBinaryJSON(result, mustParseBinaryFromString(t, test.expected))
		require.Equal(t, 0, cmp, result.String())
	}
}

func mustParseBinaryFromString(t *testing.T, s string) BinaryJSON {
	result, err := ParseBinaryJSONFromString(s)
	require.NoError(t, err)
	return result
}

func BenchmarkBinaryMarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(jsonBenchStr)))
	bj, _ := ParseBinaryJSONFromString(jsonBenchStr)
	for i := 0; i < b.N; i++ {
		_, _ = bj.MarshalJSON()
	}
}

func TestBinaryJSONContains(t *testing.T) {
	var tests = []struct {
		input    string
		target   string
		expected bool
	}{
		{`{}`, `{}`, true},
		{`{"a":1}`, `{}`, true},
		{`{"a":1}`, `1`, false},
		{`{"a":[1]}`, `[1]`, false},
		{`{"b":2, "c":3}`, `{"c":3}`, true},
		{`1`, `1`, true},
		{`[1]`, `1`, true},
		{`[1,2]`, `[1]`, true},
		{`[1,2]`, `[1,3]`, false},
		{`[1,2]`, `["1"]`, false},
		{`[1,2,[1,3]]`, `[1,3]`, true},
		{`[1,2,[1,[5,[3]]]]`, `[1,3]`, true},
		{`[1,2,[1,[5,{"a":[2,3]}]]]`, `[1,{"a":[3]}]`, true},
		{`[{"a":1}]`, `{"a":1}`, true},
		{`[{"a":1,"b":2}]`, `{"a":1}`, true},
		{`[{"a":{"a":1},"b":2}]`, `{"a":1}`, false},
	}

	for _, test := range tests {
		obj := mustParseBinaryFromString(t, test.input)
		target := mustParseBinaryFromString(t, test.target)
		require.Equal(t, test.expected, ContainsBinaryJSON(obj, target))
	}
}

func TestBinaryJSONCopy(t *testing.T) {
	expectedList := []string{
		`{"a": [1, "2", {"aa": "bb"}, 4, null], "b": true, "c": null}`,
		`{"aaaaaaaaaaa": [1, "2", {"aa": "bb"}, 4.1], "bbbbbbbbbb": true, "ccccccccc": "d"}`,
		`[{"a": 1, "b": true}, 3, 3.5, "hello, world", null, true]`,
	}
	for _, expected := range expectedList {
		result := mustParseBinaryFromString(t, expected)
		require.Equal(t, result.String(), result.Copy().String())
	}
}

func TestGetKeys(t *testing.T) {
	parsedBJ := mustParseBinaryFromString(t, "[]")
	require.Equal(t, "[]", parsedBJ.GetKeys().String())
	parsedBJ = mustParseBinaryFromString(t, "{}")
	require.Equal(t, "[]", parsedBJ.GetKeys().String())
	parsedBJ = mustParseBinaryFromString(t, "{\"comment\": \"1234\"}")
	require.Equal(t, "[\"comment\"]", parsedBJ.GetKeys().String())
	parsedBJ = mustParseBinaryFromString(t, "{\"name\": \"Tom\", \"age\": 19}")
	require.Equal(t, "[\"age\", \"name\"]", parsedBJ.GetKeys().String())
	require.Equal(t, 2, parsedBJ.GetKeys().GetElemCount())

	b := strings.Builder{}
	b.WriteString("{\"")
	for range 65536 {
		b.WriteByte('a')
	}
	b.WriteString("\": 1}")
	parsedBJ, err := ParseBinaryJSONFromString(b.String())
	require.Error(t, err)
	require.EqualError(t, err, "[types:8129]TiDB does not yet support JSON objects with the key length >= 65536")
}

func TestBinaryJSONDepth(t *testing.T) {
	var tests = []struct {
		input    string
		expected int
	}{
		{`{}`, 1},
		{`[]`, 1},
		{`true`, 1},
		{`[10, 20]`, 2},
		{`[[], {}]`, 2},
		{`[10, {"a": 20}]`, 3},
		{`{"Person": {"Name": "Homer", "Age": 39, "Hobbies": ["Eating", "Sleeping"]} }`, 4},
	}

	for _, test := range tests {
		obj := mustParseBinaryFromString(t, test.input)
		require.Equal(t, test.expected, obj.GetElemDepth())
	}
}

func TestParseBinaryFromString(t *testing.T) {
	obj, err := ParseBinaryJSONFromString("")
	require.Error(t, err)
	require.Equal(t, "", obj.String())
	require.Contains(t, err.Error(), "The document is empty")

	obj, err = ParseBinaryJSONFromString(`"a""`)
	require.Error(t, err)
	require.Equal(t, "", obj.String())
	require.Contains(t, err.Error(), "The document root must not be followed by other values.")
}

func TestCreateBinary(t *testing.T) {
	bj := CreateBinaryJSON(int64(1 << 62))
	require.Equal(t, JSONTypeCodeInt64, bj.TypeCode)
	require.NotNil(t, bj.Value)

	bj = CreateBinaryJSON(123456789.1234567)
	require.Equal(t, JSONTypeCodeFloat64, bj.TypeCode)

	bj = CreateBinaryJSON(0.00000001)
	require.Equal(t, JSONTypeCodeFloat64, bj.TypeCode)

	bj = CreateBinaryJSON(1e-20)
	require.Equal(t, JSONTypeCodeFloat64, bj.TypeCode)
	require.NotNil(t, bj.Value)

	bj2 := CreateBinaryJSON(bj)
	require.Equal(t, bj.TypeCode, bj2.TypeCode)
	require.NotNil(t, bj2.Value)

	func() {
		defer func() {
			r := recover()
			require.Regexp(t, "^unknown type:", r)
		}()
		bj = CreateBinaryJSON(int8(123))
		require.Equal(t, bj.TypeCode, bj.TypeCode)
	}()
}

func TestFunctions(t *testing.T) {
	testByte := []byte{'\\', 'b', 'f', 'n', 'r', 't', 'u', 'z', '0'}
	testOutput, err := unquoteJSONString(string(testByte))
	require.Equal(t, "\bfnrtuz0", testOutput)
	require.NoError(t, err)

	n, err := PeekBytesAsJSON(testByte)
	require.Equal(t, 0, n)
	require.EqualError(t, err, "Invalid JSON bytes")

	n, err = PeekBytesAsJSON([]byte(""))
	require.Equal(t, 0, n)
	require.EqualError(t, err, "Cant peek from empty bytes")
}

func TestBinaryJSONExtractCallback(t *testing.T) {
	bj1 := mustParseBinaryFromString(t, `{"\"hello\"": "world", "a": [1, "2", {"aa": "bb"}, 4.0, {"aa": "cc"}], "b": true, "c": ["d"]}`)
	bj2 := mustParseBinaryFromString(t, `[{"a": 1, "b": true}, 3, 3.5, "hello, world", null, true]`)

	type ExpectedPair struct {
		path string
		bj   BinaryJSON
	}

	var tests = []struct {
		bj       BinaryJSON
		pathExpr string
		expected []ExpectedPair
	}{
		{bj1, "$.a", []ExpectedPair{
			{"$.a", mustParseBinaryFromString(t, `[1, "2", {"aa": "bb"}, 4.0, {"aa": "cc"}]`)},
		}},
		{bj2, "$.a", []ExpectedPair{}},
		{bj1, "$[0]", []ExpectedPair{}}, // in extractToCallback/Walk/Search, DON'T autowraped bj as an array.
		{bj2, "$[0]", []ExpectedPair{
			{"$[0]", mustParseBinaryFromString(t, `{"a": 1, "b": true}`)},
		}},
		{bj1, "$.a[2].aa", []ExpectedPair{
			{"$.a[2].aa", mustParseBinaryFromString(t, `"bb"`)},
		}},
		{bj1, "$.a[*].aa", []ExpectedPair{
			{"$.a[2].aa", mustParseBinaryFromString(t, `"bb"`)},
			{"$.a[4].aa", mustParseBinaryFromString(t, `"cc"`)},
		}},
		{bj1, "$.*[0]", []ExpectedPair{
			// {"$.\"hello\"[0]", mustParseBinaryFromString(c, `"world"`)},  // NO autowraped as an array.
			{"$.a[0]", mustParseBinaryFromString(t, `1`)},
			// {"$.b[0]", mustParseBinaryFromString(c, `true`)},  // NO autowraped as an array.
			{"$.c[0]", mustParseBinaryFromString(t, `"d"`)},
		}},
		{bj1, `$.a[*]."aa"`, []ExpectedPair{
			{"$.a[2].aa", mustParseBinaryFromString(t, `"bb"`)},
			{"$.a[4].aa", mustParseBinaryFromString(t, `"cc"`)},
		}},
		{bj1, `$."\"hello\""`, []ExpectedPair{
			{`$."\"hello\""`, mustParseBinaryFromString(t, `"world"`)},
		}},
		{bj1, `$**[1]`, []ExpectedPair{
			{`$.a[1]`, mustParseBinaryFromString(t, `"2"`)},
		}},
	}

	for _, test := range tests {
		pe, err := ParseJSONPathExpr(test.pathExpr)
		require.NoError(t, err)

		count := 0
		cb := func(fullPath JSONPathExpression, bj BinaryJSON) (stop bool, err error) {
			require.Less(t, count, len(test.expected))
			if count < len(test.expected) {
				require.Equal(t, test.expected[count].path, fullPath.String())
				require.Equal(t, test.expected[count].bj.String(), bj.String())
			}
			count++
			return false, nil
		}

		fullPath := JSONPathExpression{legs: make([]jsonPathLeg, 0), flags: jsonPathExpressionFlag(0)}
		_, err = test.bj.extractToCallback(pe, cb, fullPath)
		require.NoError(t, err)
		require.Equal(t, len(test.expected), count)
	}
}

func TestBinaryJSONWalk(t *testing.T) {
	bj1 := mustParseBinaryFromString(t, `["abc", [{"k": "10"}, "def"], {"x":"abc"}, {"y":"bcd"}]`)
	bj2 := mustParseBinaryFromString(t, `{}`)

	type ExpectedPair struct {
		path string
		bj   BinaryJSON
	}
	var tests = []struct {
		bj       BinaryJSON
		paths    []string
		expected []ExpectedPair
	}{
		{bj1, []string{}, []ExpectedPair{
			{`$`, mustParseBinaryFromString(t, `["abc", [{"k": "10"}, "def"], {"x":"abc"}, {"y":"bcd"}]`)},
			{`$[0]`, mustParseBinaryFromString(t, `"abc"`)},
			{`$[1]`, mustParseBinaryFromString(t, `[{"k": "10"}, "def"]`)},
			{`$[1][0]`, mustParseBinaryFromString(t, `{"k": "10"}`)},
			{`$[1][0].k`, mustParseBinaryFromString(t, `"10"`)},
			{`$[1][1]`, mustParseBinaryFromString(t, `"def"`)},
			{`$[2]`, mustParseBinaryFromString(t, `{"x":"abc"}`)},
			{`$[2].x`, mustParseBinaryFromString(t, `"abc"`)},
			{`$[3]`, mustParseBinaryFromString(t, `{"y":"bcd"}`)},
			{`$[3].y`, mustParseBinaryFromString(t, `"bcd"`)},
		}},
		{bj1, []string{`$[1]`}, []ExpectedPair{
			{`$[1]`, mustParseBinaryFromString(t, `[{"k": "10"}, "def"]`)},
			{`$[1][0]`, mustParseBinaryFromString(t, `{"k": "10"}`)},
			{`$[1][0].k`, mustParseBinaryFromString(t, `"10"`)},
			{`$[1][1]`, mustParseBinaryFromString(t, `"def"`)},
		}},
		{bj1, []string{`$[1]`, `$[1]`}, []ExpectedPair{ // test for unique
			{`$[1]`, mustParseBinaryFromString(t, `[{"k": "10"}, "def"]`)},
			{`$[1][0]`, mustParseBinaryFromString(t, `{"k": "10"}`)},
			{`$[1][0].k`, mustParseBinaryFromString(t, `"10"`)},
			{`$[1][1]`, mustParseBinaryFromString(t, `"def"`)},
		}},
		{bj1, []string{`$.m`}, []ExpectedPair{}},
		{bj2, []string{}, []ExpectedPair{
			{`$`, mustParseBinaryFromString(t, `{}`)},
		}},
	}

	for _, test := range tests {
		count := 0
		cb := func(fullPath JSONPathExpression, bj BinaryJSON) (stop bool, err error) {
			require.Less(t, count, len(test.expected))
			if count < len(test.expected) {
				require.Equal(t, test.expected[count].path, fullPath.String())
				require.Equal(t, test.expected[count].bj.String(), bj.String())
			}
			count++
			return false, nil
		}

		var err error
		if len(test.paths) > 0 {
			peList := make([]JSONPathExpression, 0, len(test.paths))
			for _, path := range test.paths {
				pe, errPath := ParseJSONPathExpr(path)
				require.NoError(t, errPath)
				peList = append(peList, pe)
			}
			err = test.bj.Walk(cb, peList...)
		} else {
			err = test.bj.Walk(cb)
		}
		require.NoError(t, err)
		require.Equal(t, len(test.expected), count)
	}
}

func TestBinaryJSONOpaque(t *testing.T) {
	var tests = []struct {
		bj             BinaryJSON
		expectedOpaque Opaque
		expectedOutput string
	}{
		{
			BinaryJSON{
				TypeCode: JSONTypeCodeOpaque,
				Value:    []byte{233, 1, '9'},
			},
			Opaque{
				TypeCode: 233,
				Buf:      []byte{'9'},
			},
			`"base64:type233:OQ=="`,
		},
		{
			BinaryJSON{
				TypeCode: JSONTypeCodeOpaque,
				Value:    append([]byte{233, 0x80, 0x01}, make([]byte, 128)...),
			},
			Opaque{
				TypeCode: 233,
				Buf:      make([]byte, 128),
			},
			`"base64:type233:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="`,
		},
	}

	for _, test := range tests {
		buf := []byte{}

		require.Equal(t, test.expectedOpaque.TypeCode, test.bj.GetOpaqueFieldType())
		require.Equal(t, test.expectedOpaque, test.bj.GetOpaque())

		buf, err := test.bj.marshalTo(buf)
		require.NoError(t, err)
		require.Equal(t, string(buf), test.expectedOutput)
	}
}

func TestHashValue(t *testing.T) {
	// The following values should have different hash value
	jsons := []BinaryJSON{
		CreateBinaryJSON([]any{}),
		CreateBinaryJSON([]any{CreateBinaryJSON([]any{})}),
		CreateBinaryJSON([]any{CreateBinaryJSON([]any{CreateBinaryJSON([]any{})})}),
		CreateBinaryJSON(map[string]any{}),
		CreateBinaryJSON([]any{CreateBinaryJSON(false)}),
		CreateBinaryJSON([]any{CreateBinaryJSON(true)}),
		CreateBinaryJSON([]any{CreateBinaryJSON(nil)}),
	}

	// TODO: use a better way to count distinct json value
	counter := make(map[string]struct{})
	for _, j := range jsons {
		hashKey := j.HashValue([]byte{})
		counter[string(hashKey)] = struct{}{}
	}

	require.Equal(t, len(jsons), len(counter))
}

func FuzzJSONExtract(f *testing.F) {
	f.Add(`["abc", 5, 1.234]`, "$[0]")
	f.Add(`{"key": "value"}`, "$.key")
	f.Add(`{"key": "value"}`, "$.*")
	f.Add(`{"key": "value"}`, "$.**")
	f.Add(`"abc"`, "$")
	f.Add("5", "$")
	f.Add("1.2345", "$")

	var db *sql.DB
	testDBDSN := os.Getenv("TEST_MYSQL_DSN")
	if testDBDSN != "" {
		var err error
		db, err = sql.Open("mysql", testDBDSN)
		require.NoError(f, err)
		defer db.Close()
	}

	caseFile, err := os.OpenFile("/tmp/testcase", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		panic(err)
	}

	f.Fuzz(func(t *testing.T, jsonData string, path string) {
		if !utf8.Valid([]byte(jsonData)) || !utf8.ValidString(path) {
			// TiDB actually has different behavior with MySQL when there are invalid utf8 characters
			// ref https://github.com/pingcap/tidb/issues/58886
			t.Skip()
		}

		j, err := ParseBinaryJSONFromString(jsonData)
		if err != nil {
			// not interesting value
			t.Skip()
		}

		pathExpr, err := ParseJSONPathExpr(path)
		if err != nil {
			// not interesting value
			t.Skip()
		}

		// TODO: support multiple json path expressions
		output, found := j.Extract([]JSONPathExpression{pathExpr})
		if found {
			require.NotEqual(t, output.TypeCode, 0)
		}

		if db == nil {
			return
		}

		output.Walk(func(fullpath JSONPathExpression, bj BinaryJSON) (stop bool, err error) {
			if bj.TypeCode == JSONTypeCodeFloat64 {
				// skip the test with float point, as MySQL itself doesn't have a consistent behavior between multiple versions and
				// in different situation.
				t.Skip()
			}

			// skip test case with invalid unicode
			if bj.TypeCode == JSONTypeCodeString && strings.ContainsRune(string(bj.GetString()), utf8.RuneError) {
				t.Skip()
			}
			if bj.TypeCode == JSONTypeCodeObject {
				count := bj.GetElemCount()
				for i := range count {
					key := string(bj.objectGetKey(i))
					if strings.ContainsRune(key, utf8.RuneError) {
						t.Skip()
					}
				}
			}

			return false, nil
		})

		fmt.Fprintf(caseFile, "jsonData: %s, path: %s\n", jsonData, path)

		// validate the result is the same with MySQL
		conn, err := db.Conn(context.Background())
		require.NoError(t, err)
		defer conn.Close()

		rows, err := conn.QueryContext(context.Background(), `SELECT JSON_EXTRACT(?, ?)`, jsonData, path)
		require.NoError(t, err)
		defer rows.Close()

		var mysqlResult *string
		rows.Next()
		require.NoError(t, rows.Scan(&mysqlResult))
		require.NoError(t, rows.Err())

		if mysqlResult == nil {
			require.False(t, found)
			// not interesting value
			t.Skip()
		}

		require.Equal(t, *mysqlResult, output.String(), "Expect %v, got %v", []byte(*mysqlResult), []byte(output.String()))
	})
}
