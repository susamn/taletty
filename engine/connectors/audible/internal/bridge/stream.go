// Package bridge contains the Audible connector's private wire/process boundary.
package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const (
	Version       = 1
	MaxFrameBytes = 4 << 20
)

var ErrProtocol = errors.New("invalid bridge protocol stream")

type Kind string

const (
	Stage    Kind = "stage"
	Progress Kind = "progress"
	Result   Kind = "result"
	Failure  Kind = "error"
)

// Frame is an envelope, NOT a validated operation event. Raw is an owned copy;
// action-specific validation/conversion must precede any public model or UI use.
// Never log/forward Raw, provider messages, decoder errors or stderr to the app.
type Frame struct {
	Protocol    int
	OperationID string
	Seq         uint64
	Type        Kind
	Raw         json.RawMessage
}

// ReadStream validates v1 envelopes and returns a candidate terminal only after
// clean EOF. EOF is NOT successful process exit: the process owner must also wait
// and check exit status before trusting it. No partial library snapshot is implied.
//
// visit is an optional synchronous internal payload validator/consumer, not the
// public reporter. It sees provisional stage/progress only, may reject a payload,
// and must return promptly. Its errors/panics propagate unchanged. Terminal action
// payload validation remains the caller's responsibility, even with a nil visit.
//
// The caller owns reader cancellation/closing and process cleanup. This function
// cannot interrupt a blocked Reader or callback and starts no goroutines/processes.
func ReadStream(r io.Reader, operationID string, visit func(Frame) error) (Frame, error) {
	if operationID == "" {
		return Frame{}, ErrProtocol
	}
	scanner := bufio.NewScanner(r)
	// Leave room for the line ending, then enforce the exact content bound. The
	// standard ScanLines splitter accepts LF/CRLF and an EOF-terminated final line.
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes+2)
	var terminal Frame
	var seq uint64
	for scanner.Scan() {
		raw := scanner.Bytes()
		if terminal.Type != "" || len(raw) > MaxFrameBytes || !utf8.Valid(raw) {
			return Frame{}, ErrProtocol
		}
		frame, err := decodeEnvelope(raw)
		if err != nil {
			return Frame{}, err
		}
		if frame.Protocol != Version || frame.OperationID != operationID || frame.Seq == 0 || frame.Seq != seq+1 {
			return Frame{}, ErrProtocol
		}
		seq = frame.Seq
		switch frame.Type {
		case Result, Failure:
			terminal = frame
		case Stage, Progress:
			if visit != nil {
				if err := visit(frame); err != nil {
					return Frame{}, err
				}
			}
		default:
			return Frame{}, ErrProtocol
		}
	}
	if scanner.Err() != nil || terminal.Type == "" {
		return Frame{}, ErrProtocol
	}
	return terminal, nil
}

// Decode exact header names (not encoding/json's case-insensitive struct aliases)
// and reject ambiguous duplicate top-level keys, including escaped spellings.
// Unknown optional fields are skipped, not interpreted as defaults or public data.
func decodeEnvelope(raw []byte) (Frame, error) {
	var frame Frame
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return Frame{}, ErrProtocol
	}
	fields := map[string]any{
		"protocol": &frame.Protocol, "operation_id": &frame.OperationID,
		"seq": &frame.Seq, "type": &frame.Type,
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return Frame{}, ErrProtocol
		}
		key := token.(string) // Decoder requires string object keys after '{'.
		if seen[key] {
			return Frame{}, ErrProtocol
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return Frame{}, ErrProtocol
		}
		if target, known := fields[key]; known {
			if err := json.Unmarshal(value, target); err != nil {
				return Frame{}, ErrProtocol
			}
		}
	}
	if _, err := decoder.Token(); err != nil { // Closing object delimiter.
		return Frame{}, ErrProtocol
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return Frame{}, ErrProtocol
	}
	frame.Raw = append(json.RawMessage(nil), raw...)
	return frame, nil
}
