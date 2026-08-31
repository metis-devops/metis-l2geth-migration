package strictio

import "testing"

func FuzzStrictJSON(f *testing.F) {
	type document struct {
		Value string `json:"value"`
		Count uint64 `json:"count"`
	}
	f.Add([]byte(`{"value":"seed","count":1}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if int64(len(data)) > MaxMetadataSize {
			return
		}
		_, _ = DecodeJSON[document](data, "fuzz document")
	})
}
