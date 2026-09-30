package jsonpos

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestPosition(t *testing.T) {
	data := []byte("[\n  \"é\", 1\n]")
	for _, tt := range []struct {
		offset       int64
		line, column int
	}{
		{0, 1, 1},
		{2, 2, 1},
		{4, 2, 3},
		{10, 2, 8}, // the 1: columns count characters, not bytes
		{-5, 1, 1},
		{100, 3, 2},
	} {
		if line, column := Position(data, tt.offset); line != tt.line || column != tt.column {
			t.Errorf("Position(%d) = %d, %d; want %d, %d", tt.offset, line, column, tt.line, tt.column)
		}
	}
}

type record struct {
	Name  string   `json:"name"`
	Count int      `json:"count"`
	Tags  []string `json:"tags"`
}

// TestOffset: a type error points at the start of the value that does not
// fit, where encoding/json points just past it.
func TestOffset(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
	}{
		{"string for int", "[\n  {\"name\": \"a\"},\n  {\"name\": \"b\", \"count\": \"two\"}\n]", "line 3, column 26"},
		{"object for int", `[{"count": {"a": 1}}]`, "line 1, column 12"},
		{"object for array", `[{"tags": {}}]`, "line 1, column 11"},
		{"leading whitespace", "\n\n  [{\"count\": 1.5}]", "line 3, column 14"},
		{"syntax", "[\n  {\"name\": \"a\",}\n]", "line 2, column 16"},
	} {
		data := []byte(tt.input)
		value := bytes.TrimLeft(data, " \t\r\n")
		base := int64(len(data) - len(value))
		var records []record
		err := json.NewDecoder(bytes.NewReader(value)).Decode(&records)
		offset, ok := Offset(data, base, err)
		if got := At(data, offset, err).Error(); !ok || !strings.HasPrefix(got, tt.want+": ") {
			t.Errorf("%s: %q (located %v), want it to start with %q", tt.name, got, ok, tt.want)
		}
	}
	if _, ok := Offset(nil, 0, io.ErrUnexpectedEOF); ok {
		t.Error("an unexpected end of input points nowhere")
	}
	if _, ok := Offset(nil, 0, errors.New("other")); ok {
		t.Error("an error of another kind points nowhere")
	}
}

func TestValueStart(t *testing.T) {
	data := []byte(`{"a": [1, {"b": "c"}], "d": true}`)
	for end, want := range map[int64]int64{
		1:  0,  // {
		7:  6,  // [
		8:  7,  // 1
		11: 10, // {
		19: 16, // "c"
		32: 28, // true
		5:  5,  // no value ends there
		99: 99,
	} {
		if got := ValueStart(data, end); got != want {
			t.Errorf("ValueStart(%d) = %d, want %d", end, got, want)
		}
	}
}
