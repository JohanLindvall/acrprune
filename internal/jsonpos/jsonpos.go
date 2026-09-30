// Package jsonpos places encoding/json's errors in the document they come
// from, by line and column, for messages about hand-edited files.
package jsonpos

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Position returns the 1-based line and column, counting characters, of
// offset in data. An offset outside data is clamped to it.
func Position(data []byte, offset int64) (line, column int) {
	offset = min(max(offset, 0), int64(len(data)))
	before := data[:offset]
	lineStart := bytes.LastIndexByte(before, '\n') + 1
	return bytes.Count(before, []byte{'\n'}) + 1, utf8.RuneCount(before[lineStart:]) + 1
}

// At returns err prefixed with the line and column of offset in data.
func At(data []byte, offset int64, err error) error {
	line, column := Position(data, offset)
	return fmt.Errorf("line %d, column %d: %w", line, column, err)
}

// Offset returns the offset in data that err, from decoding data[base:] with
// encoding/json, points at: the offending byte of a syntax error, or the
// start of the value a type error could not store. ok is false for an error
// that points nowhere, such as an unexpected end of input.
//
// A json.Decoder's type errors count from the start of the value it decodes,
// which is base only when data[base:] starts with the value itself, not with
// whitespace: trim it first.
func Offset(data []byte, base int64, err error) (offset int64, ok bool) {
	if syntaxErr, ok := errors.AsType[*json.SyntaxError](err); ok {
		return base + syntaxErr.Offset - 1, true // the offset counts the offending byte
	}
	if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		return ValueStart(data, base+typeErr.Offset), true
	}
	return 0, false
}

// ValueStart returns the offset at which the value ending at end starts,
// given end as a type error reports it: just past a scalar, or just past the
// opening bracket of an object or array. data must be valid JSON up to end.
// When no value ends there, ValueStart returns end.
func ValueStart(data []byte, end int64) int64 {
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		start := TokenStart(data, dec.InputOffset())
		if _, err := dec.Token(); err != nil || dec.InputOffset() > end {
			return end
		}
		if dec.InputOffset() == end {
			return start
		}
	}
}

// TokenStart returns the offset of the first token at or after offset, when
// offset is where a json.Decoder's InputOffset leaves off: the end of the
// previous token, before the whitespace and the separators (',' and ':') its
// Token method consumes unseen.
func TokenStart(data []byte, offset int64) int64 {
	for offset < int64(len(data)) && strings.IndexByte(" \t\r\n,:", data[offset]) >= 0 {
		offset++
	}
	return offset
}
