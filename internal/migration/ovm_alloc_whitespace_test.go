package migration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ethereum/go-ethereum/common"
)

// Generate whitespace without allocating an input-sized string or buffer.
type allocSpaces struct {
	remaining int
	reads     int
	afterRead func(int)
}

func (r *allocSpaces) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range n {
		p[i] = " \t\r\n"[i%4]
	}
	r.remaining -= n
	r.reads++
	if r.afterRead != nil {
		r.afterRead(r.reads)
	}
	return n, nil
}

type allocDeliveredReader struct {
	io.Reader
	bytes int
}

func (r *allocDeliveredReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += n
	return n, err
}

func TestOVMGenesisAllocWhitespaceDecoderBound(t *testing.T) {
	for _, size := range []int{1 << 20, 16 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			input := io.MultiReader(&allocSpaces{remaining: size}, strings.NewReader("{"),
				&allocSpaces{remaining: size}, strings.NewReader("}"), &allocSpaces{remaining: size})
			// Observe what reaches the real JSON decoder, without depending on its
			// private buffer fields or on process-wide allocation/RSS estimates.
			delivered := &allocDeliveredReader{Reader: &ovmAllocReader{ctx: t.Context(), r: input}}
			decoder := json.NewDecoder(delivered)
			if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
				t.Fatalf("opening object: %v %v", token, err)
			}
			if decoder.More() {
				t.Fatal("unexpected object member")
			}
			if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
				t.Fatalf("closing object: %v %v", token, err)
			}
			if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
				t.Fatalf("trailing whitespace: %v", err)
			}
			if delivered.bytes != len(" { } ") {
				t.Fatalf("whitespace reached decoder unbounded: %d bytes", delivered.bytes)
			}
		})
	}
}

func TestOVMGenesisAllocWhitespaceDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alloc.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const size = 16 << 20
	prefix := `{"0000000000000000000000000000000000000001":{"balance":`
	suffix := `7,"code":"0x00","storage":{"01":"02"}}}`
	input := io.MultiReader(&allocSpaces{remaining: size}, strings.NewReader(prefix),
		&allocSpaces{remaining: size}, strings.NewReader(suffix), &allocSpaces{remaining: size})
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), input)
	if err := errors.Join(copyErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	expected := common.BytesToHash(h.Sum(nil))
	loaded, err := loadOVMGenesisAlloc(t.Context(), path, testAllocIndex(t))
	if err != nil || loaded != expected {
		t.Fatalf("load lost raw input digest: %s want %s: %v", loaded, expected, err)
	}
	confirmed, err := hashOVMGenesisAlloc(t.Context(), path)
	if err != nil || confirmed != expected {
		t.Fatalf("confirmation lost raw input digest: %s want %s: %v", confirmed, expected, err)
	}
	compact, err := loadOVMGenesisAlloc(t.Context(), writeAllocFile(t, prefix+suffix), testAllocIndex(t))
	if err != nil || compact == expected {
		t.Fatalf("distinct whitespace must change the digest: %v", err)
	}
	if err := confirmOVMAllocEvidence(t.Context(), path, &OVMGenesisAllocEvidence{FileSHA256: compact}); err == nil {
		t.Fatal("whitespace-only input change was accepted")
	}
}

func TestOVMGenesisAllocWhitespacePreservesGrammar(t *testing.T) {
	// Quoted spaces, escaped quotes and escaped backslashes must survive even
	// when every byte arrives in a separate read. Keep whitespace between tokens.
	body := `{"s": "a  b\"  c\\  d\u0020e", "n": 1}`
	input := "\t\n" + body + "\r\n"
	for _, oneByte := range []bool{false, true} {
		var source io.Reader = strings.NewReader(input)
		if oneByte {
			source = iotest.OneByteReader(source)
		}
		data, err := io.ReadAll(&ovmAllocReader{ctx: t.Context(), r: source})
		if err != nil || string(data) != " "+body+" " {
			t.Fatalf("changed string/token contents (one byte=%t): %q %v", oneByte, data, err)
		}
	}
	for _, fields := range []string{`"balance":1 2`, `"balance":- 1`, `"code":"0x60  01"`, `"storage":{"0  1":"01"}`, "\"code\":\"0x60\n01\""} {
		input := `{"0000000000000000000000000000000000000001":{` + fields + `}}`
		if _, err := loadOVMGenesisAlloc(t.Context(), writeAllocFile(t, input), testAllocIndex(t)); err == nil {
			t.Fatalf("whitespace filtering repaired invalid JSON/hex: %s", fields)
		}
	}
}

func TestOVMGenesisAllocWhitespaceCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	spaces := &allocSpaces{remaining: 64 << 20, afterRead: func(reads int) {
		if reads == 5 {
			cancel()
		}
	}}
	_, err := io.Copy(io.Discard, &ovmAllocReader{ctx: ctx, r: spaces})
	if !errors.Is(err, context.Canceled) || spaces.reads != 5 {
		t.Fatalf("whitespace-only chunks delayed cancellation: reads=%d err=%v", spaces.reads, err)
	}
}

type allocReadError struct {
	data string
	err  error
}

func (r *allocReadError) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func TestOVMGenesisAllocWhitespacePreservesReadErrors(t *testing.T) {
	injected := errors.New("alloc input read failed")
	for _, data := range []string{"{}", "{} \t\n", " \t\n"} {
		reader := &ovmAllocReader{ctx: t.Context(), r: &allocReadError{data: data, err: injected}}
		decoder := json.NewDecoder(reader)
		for {
			if _, err := decoder.Token(); err != nil {
				if !errors.Is(err, injected) {
					t.Fatalf("read error lost for %q: %v", data, err)
				}
				break
			}
		}
	}
}
