package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func wire(seq int, kind Kind) string {
	return fmt.Sprintf(`{"protocol":1,"operation_id":"op","seq":%d,"type":%q}`, seq, kind)
}

func TestFragmentedStreamAndOwnedFrames(t *testing.T) {
	var lines []string
	for seq := 1; seq <= 1000; seq++ {
		kind := Progress
		if seq == 1 {
			kind = Stage
		}
		lines = append(lines, wire(seq, kind))
	}
	last := wire(1001, Result)
	lines = append(lines, last)
	for name, wrap := range map[string]func(io.Reader) io.Reader{
		"normal":   func(r io.Reader) io.Reader { return r },
		"one-byte": iotest.OneByteReader,
		"half":     iotest.HalfReader,
	} {
		t.Run(name, func(t *testing.T) {
			var received []Frame
			terminal, err := ReadStream(wrap(strings.NewReader(strings.Join(lines, "\r\n"))), "op", func(frame Frame) error {
				received = append(received, frame)
				return nil
			})
			if err != nil || terminal.Type != Result || terminal.Seq != 1001 || string(terminal.Raw) != last || len(received) != 1000 {
				t.Fatal("invalid terminal/delivery", terminal, len(received), err)
			}
			// Check after the entire stream: Scanner's backing buffer was reused.
			for i, frame := range received {
				if frame.Seq != uint64(i+1) || string(frame.Raw) != lines[i] {
					t.Fatal("frame mutated or reordered", i)
				}
			}
		})
	}
}

func TestOptionalFieldsAndTerminalKinds(t *testing.T) {
	for _, kind := range []Kind{Result, Failure} {
		// Unknown optional values, case aliases, escaped newlines and UTF-8 must
		// not change the exact required headers. Payload is intentionally opaque.
		raw := strings.TrimSuffix(wire(1, kind), "}") + `,"PROTOCOL":9,"future":{"title":"本\ntext","array":[null,true,3]}}`
		for _, ending := range []string{"", "\n", "\r\n"} {
			frame, err := ReadStream(iotest.OneByteReader(strings.NewReader(raw+ending)), "op", nil)
			if err != nil || frame.Protocol != Version || frame.OperationID != "op" || frame.Type != kind || string(frame.Raw) != raw {
				t.Fatal("optional field/ending rejected", err)
			}
		}
	}
}

func TestRejectInvalidStreams(t *testing.T) {
	terminal := wire(1, Result)
	cases := map[string]string{
		"empty": "", "blank": "\n", "non-json": "fixture-private-do-not-log",
		"null": "null", "array": "[]", "scalar": "1", "string": `"text"`,
		"byte-order-mark": "\ufeff" + terminal,
		"invalid-utf8":    strings.TrimSuffix(terminal, "}") + ",\"extra\":\"\xff\"}",
		"wrong-close":     "{]",
		"open-object":     "{", "unclosed": strings.TrimSuffix(terminal, "}"),
		"missing-comma":  strings.Replace(terminal, `,"seq"`, ` "seq"`, 1),
		"bad-value":      strings.Replace(terminal, `"seq":1`, `"seq":!`, 1),
		"truncated-json": terminal[:len(terminal)-3],
		"multiple-json":  terminal + " {}", "trailing-garbage": terminal + "SECRET_FIXTURE",
		"missing-protocol": strings.Replace(terminal, `"protocol":1,`, "", 1),
		"missing-id":       strings.Replace(terminal, `"operation_id":"op",`, "", 1),
		"missing-seq":      strings.Replace(terminal, `"seq":1,`, "", 1),
		"missing-type":     strings.Replace(terminal, `,"type":"result"`, "", 1),
		"wrong-version":    strings.Replace(terminal, `"protocol":1`, `"protocol":2`, 1),
		"string-version":   strings.Replace(terminal, `"protocol":1`, `"protocol":"1"`, 1),
		"fraction-version": strings.Replace(terminal, `"protocol":1`, `"protocol":1.0`, 1),
		"null-version":     strings.Replace(terminal, `"protocol":1`, `"protocol":null`, 1),
		"wrong-id":         strings.Replace(terminal, `"op"`, `"other"`, 1),
		"numeric-id":       strings.Replace(terminal, `"op"`, `5`, 1),
		"empty-id":         strings.Replace(terminal, `"op"`, `""`, 1),
		"null-id":          strings.Replace(terminal, `"op"`, `null`, 1),
		"zero-seq":         wire(0, Result), "first-gap": wire(2, Result),
		"negative-seq":            wire(-1, Result),
		"fraction-seq":            strings.Replace(terminal, `"seq":1`, `"seq":1.5`, 1),
		"overflow-seq":            strings.Replace(terminal, `"seq":1`, `"seq":18446744073709551616`, 1),
		"maximum-seq":             strings.Replace(terminal, `"seq":1`, `"seq":18446744073709551615`, 1),
		"null-seq":                strings.Replace(terminal, `"seq":1`, `"seq":null`, 1),
		"unknown-type":            wire(1, "future"),
		"numeric-type":            strings.Replace(terminal, `"result"`, `1`, 1),
		"null-type":               strings.Replace(terminal, `"result"`, `null`, 1),
		"case-only-header":        strings.Replace(terminal, `"protocol"`, `"PROTOCOL"`, 1),
		"duplicate-header":        strings.TrimSuffix(terminal, "}") + `,"protocol":1}`,
		"escaped-duplicate":       strings.TrimSuffix(terminal, "}") + `,"prot\u006fcol":1}`,
		"duplicate-optional":      strings.TrimSuffix(terminal, "}") + `,"x":1,"x":2}`,
		"no-terminal":             wire(1, Stage),
		"duplicate-seq":           wire(1, Stage) + "\n" + terminal,
		"middle-gap":              wire(1, Stage) + "\n" + wire(3, Result),
		"duplicate-result":        terminal + "\n" + wire(2, Result),
		"error-after-result":      terminal + "\n" + wire(2, Failure),
		"result-after-error":      wire(1, Failure) + "\n" + wire(2, Result),
		"progress-after-terminal": terminal + "\n" + wire(2, Progress),
		"trailing-blank":          terminal + "\n\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			frame, err := ReadStream(strings.NewReader(input), "op", nil)
			if err != ErrProtocol || !reflect.DeepEqual(frame, Frame{}) {
				t.Fatal("invalid stream trusted or unsafe error", frame, err)
			}
		})
	}
}

func TestExactFrameBound(t *testing.T) {
	for _, length := range []int{MaxFrameBytes - 1, MaxFrameBytes, MaxFrameBytes + 1, 2 * MaxFrameBytes} {
		// Leading JSON whitespace is still content, not trimmed out of the limit.
		raw := strings.Repeat(" ", length-len(wire(1, Result))) + wire(1, Result)
		for _, ending := range []string{"", "\n", "\r\n"} {
			frame, err := ReadStream(strings.NewReader(raw+ending), "op", nil)
			if length <= MaxFrameBytes {
				if err != nil || len(frame.Raw) != length {
					t.Fatal("bounded frame rejected", length, ending, err)
				}
			} else if err != ErrProtocol || frame.Raw != nil {
				t.Fatal("oversized frame trusted", length, ending, err)
			}
		}
	}
	// Also reject an unbounded line that never contains a valid JSON object.
	if _, err := ReadStream(strings.NewReader(strings.Repeat("x", 2*MaxFrameBytes)), "op", nil); err != ErrProtocol {
		t.Fatal(err)
	}
}

type failedReader struct{ reads int }

func (r *failedReader) Read([]byte) (int, error) {
	r.reads++
	return 0, errors.New("fixture-private-read-error")
}

func TestReaderFailureAndInvalidExpectedID(t *testing.T) {
	reader := &failedReader{}
	if _, err := ReadStream(reader, "", nil); err != ErrProtocol || reader.reads != 0 {
		t.Fatal("invalid expected ID read input", err, reader.reads)
	}
	for _, prefix := range []string{"", wire(1, Stage) + "\n", wire(1, Result) + "\n"} {
		frame, err := ReadStream(io.MultiReader(strings.NewReader(prefix), reader), "op", nil)
		if err != ErrProtocol || frame.Raw != nil {
			t.Fatal("read failure leaked or terminal trusted before EOF", frame, err)
		}
	}
	// Exercise Reader returning data and a non-EOF error in the SAME Read call.
	frame, err := ReadStream(iotest.DataErrReader(io.MultiReader(strings.NewReader(wire(1, Result)+"\n"), reader)), "op", nil)
	if err != ErrProtocol || frame.Raw != nil {
		t.Fatal("data+error trusted", frame, err)
	}
}

func TestProvisionalConsumerFailureAndPanic(t *testing.T) {
	input := wire(1, Stage) + "\n" + wire(2, Result)
	failure := errors.New("payload validator rejected fixture")
	calls := 0
	frame, err := ReadStream(strings.NewReader(input), "op", func(frame Frame) error {
		calls++
		if frame.Type != Stage {
			t.Fatal("terminal delivered provisionally")
		}
		return failure
	})
	if err != failure || frame.Raw != nil || calls != 1 {
		t.Fatal("consumer failure not preserved", frame, err, calls)
	}
	func() {
		defer func() {
			if recover() != failure {
				t.Fatal("consumer panic changed")
			}
		}()
		_, _ = ReadStream(strings.NewReader(input), "op", func(Frame) error { panic(failure) })
	}()
	calls = 0
	frame, err = ReadStream(strings.NewReader(wire(1, Stage)+"\n{"), "op", func(Frame) error { calls++; return nil })
	if err != ErrProtocol || frame.Raw != nil || calls != 1 {
		t.Fatal("partial stream trusted", frame, err, calls)
	}
}

func FuzzReadStream(f *testing.F) {
	for _, seed := range []string{"", "{", "null", wire(1, Result), wire(1, Stage) + "\n" + wire(2, Result), wire(1, Failure), `{"x":1,"x":2}`, "\xff"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var seq uint64
		frame, err := ReadStream(bytes.NewReader(data), "op", func(frame Frame) error {
			if frame.Seq != seq+1 || (frame.Type != Stage && frame.Type != Progress) || !json.Valid(frame.Raw) {
				t.Fatal("invalid provisional envelope")
			}
			seq = frame.Seq
			return nil
		})
		if err == nil {
			if frame.Protocol != Version || frame.OperationID != "op" || frame.Seq != seq+1 || (frame.Type != Result && frame.Type != Failure) || len(frame.Raw) > MaxFrameBytes || !json.Valid(frame.Raw) {
				t.Fatal("invalid candidate terminal")
			}
		} else if err != ErrProtocol || frame.Raw != nil {
			t.Fatal("unsafe failure")
		}
	})
}
