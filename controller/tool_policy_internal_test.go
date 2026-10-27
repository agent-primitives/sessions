package controller

import (
	"reflect"
	"testing"
)

// plainByte is a named byte type without marshal methods, which encoding/json writes as base64.
type plainByte uint8

// A nil slice may be journaled as null or empty, and as "" only when encoding/json would write
// base64 text for its type (structpb writes a nil []byte as ""); any other slice journaled as ""
// is a mismatch.
func TestJournalHoldsNilSlice(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
		j    any
		want bool
	}{
		{"bytes as empty text", []byte(nil), "", true},
		{"bytes as null", []byte(nil), nil, true},
		{"named bytes as empty text", []plainByte(nil), "", true},
		{"any as empty list", []any(nil), []any{}, true},
		{"strings as null", []string(nil), nil, true},
		{"strings as empty text", []string(nil), "", false},
		{"any as empty text", []any(nil), "", false},
		{"marshaler bytes as empty text", []byteMarshaler(nil), "", false},
		{"bytes as a non-empty list", []byte(nil), []any{1.0}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := journalHolds(reflect.ValueOf(tc.v), tc.j); got != tc.want {
				t.Fatalf("journalHolds(%#v, %#v) = %v, want %v", tc.v, tc.j, got, tc.want)
			}
		})
	}
}

// byteMarshaler is a byte type with a MarshalJSON method, so a slice of it is a JSON array.
type byteMarshaler uint8

func (byteMarshaler) MarshalJSON() ([]byte, error) { return []byte("1"), nil }

// writtenOne is an integer type whose MarshalJSON writes 1 whatever its value.
type writtenOne int64

func (writtenOne) MarshalJSON() ([]byte, error) { return []byte("1"), nil }
