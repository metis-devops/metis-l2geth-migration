package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
)

// Frozen pre-optimization decoder: keep its independent field walk followed by
// strict struct decoding as an acceptance and value oracle for the single pass.
func decodeOVMWitnessReference(data []byte) (ovmWitnessRecord, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ovmWitnessRecord{}, errors.New("OVM witness must be a JSON object")
	}
	seen := make(map[string]bool, 4)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return ovmWitnessRecord{}, err
		}
		key, ok := token.(string)
		if !ok {
			return ovmWitnessRecord{}, errors.New("invalid witness field")
		}
		if seen[key] {
			return ovmWitnessRecord{}, fmt.Errorf("duplicate OVM witness field %q", key)
		}
		if key != "type" && key != "address" && key != "owner" && key != "spender" {
			return ovmWitnessRecord{}, fmt.Errorf("unknown OVM witness field %q", key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return ovmWitnessRecord{}, err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ovmWitnessRecord{}, errors.New("null OVM witness field")
		}
	}
	return strictio.DecodeJSON[ovmWitnessRecord](data, "OVM witness record")
}

var witnessDecodeSeeds = []string{
	`{"type":"address","address":"0x0000000000000000000000000000000000000001"}`,
	`{"spender":"0x0000000000000000000000000000000000000002","type":"allowance","owner":"0x0000000000000000000000000000000000000001"}`,
	`{"type":"address","addr\u0065ss":"0x0000000000000000000000000000000000000001"}`,
	`{"type":"address","type":"allowance"}`,
	`{"type":"address","ty\u0070e":"allowance"}`,
	`{"type":"address","Type":"allowance"}`,
	`{"type":null}`, `{"address":null}`, `{"owner":null}`, `{"spender":null}`,
	`{"type":1}`, `{"address":true}`, `{"owner":[]}`, `{"spender":{}}`,
	`{"type":"address","address":"0x01"}`, `{"balance":1}`,
	`{"type":"address"} {}`, `{"type":"address"} true`, `{"type":"address"} x`,
	`{"type":"address"`, `{"type":`, `{"address":"x",}`, `{`, `}`, `[]`, `null`, ``, `{}`,
	" \t{\"type\":\"address\"} \r\n",
}

func TestOVMWitnessDecodeMatchesReference(t *testing.T) {
	for _, input := range witnessDecodeSeeds {
		t.Run(input, func(t *testing.T) { assertWitnessDecodeEquivalent(t, []byte(input)) })
	}
}

func assertWitnessDecodeEquivalent(t testing.TB, data []byte) {
	t.Helper()
	want, wantErr := decodeOVMWitnessReference(data)
	got, gotErr := decodeOVMWitness(data)
	if (wantErr == nil) != (gotErr == nil) || (wantErr == nil && !reflect.DeepEqual(want, got)) {
		t.Fatalf("decode %q: reference=%+v err=%v; single-pass=%+v err=%v", data, want, wantErr, got, gotErr)
	}
}

func FuzzOVMWitnessDecode(f *testing.F) {
	for _, input := range witnessDecodeSeeds {
		f.Add([]byte(input))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4095 {
			t.Skip()
		} // A witness line cannot exceed the scanner bound.
		assertWitnessDecodeEquivalent(t, data)
	})
}

func BenchmarkOVMWitnessDecode(b *testing.B) {
	for _, kind := range []struct{ name, input string }{
		{"address", witnessDecodeSeeds[0]}, {"allowance", witnessDecodeSeeds[1]},
	} {
		for _, decoder := range []struct {
			name   string
			decode func([]byte) (ovmWitnessRecord, error)
		}{
			{"reference", decodeOVMWitnessReference}, {"single-pass", decodeOVMWitness},
		} {
			b.Run(kind.name+"/"+decoder.name, func(b *testing.B) {
				data := []byte(kind.input)
				b.ReportAllocs()
				for b.Loop() {
					if _, err := decoder.decode(data); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
