// Package audible implements the Audible connector, separate from neutral models.
package audible

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
)

//go:embed internal/bridge/adapter/doctor.py
var doctorSource string

var (
	ErrRuntimeMissing      = errors.New("Python runtime could not start")
	ErrRuntimeIncompatible = errors.New("Python bridge readiness could not be validated")
)

type Readiness string

const (
	Ready               Readiness = "ready"
	RuntimeMissing      Readiness = "runtime_missing"
	RuntimeIncompatible Readiness = "runtime_incompatible"
	NotChecked          Readiness = "not_checked"
)

// Diagnostics contains fixed readiness states, never raw output/paths/exceptions.
// This is not authentication, codec/media or release-platform readiness evidence.
type Diagnostics struct {
	Python  Readiness `json:"python"`
	Audible Readiness `json:"audible"`
	FFmpeg  Readiness `json:"ffmpeg"`
	FFprobe Readiness `json:"ffprobe"`
}

func (d Diagnostics) Ready() bool {
	return d.Python == Ready && d.Audible == Ready && d.FFmpeg == Ready && d.FFprobe == Ready
}

// Doctor performs a read-only check using an explicitly supplied Python executable.
// It does not discover/install runtimes or acquire/read/create a data/auth root.
// CPython 3.12 is evidenced here; installation/platform policy remains separate.
func Doctor(ctx context.Context, python string) (Diagnostics, error) {
	return doctor(ctx, python, doctorSource)
}

func doctor(ctx context.Context, python, source string) (Diagnostics, error) {
	id := "doctor-" + rand.Text()
	request := []byte(`{"protocol":1,"operation_id":"` + id + `","action":"doctor"}`)
	frame, err := bridge.Run(ctx, python, []string{"-I", "-B", "-c", source}, request, id, nil)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Diagnostics{}, err
		}
		if errors.Is(err, bridge.ErrStart) {
			return Diagnostics{}, ErrRuntimeMissing
		}
		return Diagnostics{}, ErrRuntimeIncompatible
	}
	if frame.Type != bridge.Result || frame.Seq != 1 {
		return Diagnostics{}, ErrRuntimeIncompatible
	}
	// Run already validates UTF-8/JSON and duplicate top-level keys. Use exact map
	// keys, not encoding/json's case-insensitive struct-field aliases.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(frame.Raw, &fields); err != nil {
		return Diagnostics{}, ErrRuntimeIncompatible
	}
	var result Diagnostics
	for name, target := range map[string]*Readiness{
		"python": &result.Python, "audible": &result.Audible,
		"ffmpeg": &result.FFmpeg, "ffprobe": &result.FFprobe,
	} {
		if err := json.Unmarshal(fields[name], target); err != nil || !validReadiness(*target) {
			return Diagnostics{}, ErrRuntimeIncompatible
		}
	}
	// Unsupported Python skips all other checks; compatible Python checks all.
	if result.Python == RuntimeIncompatible {
		if result.Audible != NotChecked || result.FFmpeg != NotChecked || result.FFprobe != NotChecked {
			return Diagnostics{}, ErrRuntimeIncompatible
		}
	} else if result.Python != Ready || result.Audible == NotChecked || result.FFmpeg == NotChecked || result.FFprobe == NotChecked {
		return Diagnostics{}, ErrRuntimeIncompatible
	}
	return result, nil
}

func validReadiness(state Readiness) bool {
	switch state {
	case Ready, RuntimeMissing, RuntimeIncompatible, NotChecked:
		return true
	default:
		return false
	}
}
