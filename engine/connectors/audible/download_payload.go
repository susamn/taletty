package audible

import (
	"bytes"
	"encoding/json"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
	"github.com/susamn/taletty/engine/model"
)

var downloadFailures = map[string]string{
	"storage_failed":     "Private media storage failed.",
	"storage_uncertain":  "Media published; durability not confirmed.",
	"conflict":           "Completed media conflicts with this download.",
	"preparation_failed": "Media preparation failed.",
	"disk_full":          "Insufficient space for media.",
	"permission_denied":  "Media access was refused.",
}

func downloadFailure(code, id string) *model.Failure {
	if message, ok := downloadFailures[code]; ok {
		return &model.Failure{Code: code, Message: message, Retry: "none", OperationID: id}
	}
	result := libraryFailure(code, id)
	if code == "invalid_input" {
		result.Message = "Invalid download request."
	}
	if code == "protocol_error" {
		result.Message = "Audible download response was invalid."
	}
	return result
}
func knownDownloadCode(code string) bool {
	_, shared := libraryFailures[code]
	_, media := downloadFailures[code]
	return shared || media
}

type downloadOutcome struct {
	ready, reused       bool
	published           *bool
	mediaCode, authCode *string
	chapters            []model.Chapter
}

// All nullable fields are required: absence is not an invented false/unknown.
func nullable(fields map[string]json.RawMessage, key string, target any) error {
	raw, ok := fields[key]
	if !ok || json.Unmarshal(raw, target) != nil {
		return bridge.ErrProtocol
	}
	return nil
}
func downloadTerminal(frame bridge.Frame) (downloadOutcome, error) {
	var out downloadOutcome
	// A chapter node contributes an object AND a children array to JSON depth.
	fields, err := strictFieldsBound(frame.Raw, 2*model.MaxChapterDepth+8)
	if err != nil {
		return out, err
	}
	if frame.Type == bridge.Failure {
		failure, err := object(fields["failure"])
		var code string
		if err != nil || required(failure, "code", &code) != nil || !knownDownloadCode(code) {
			return out, bridge.ErrProtocol
		}
		return out, downloadFailure(code, "")
	}
	if frame.Type != bridge.Result {
		return out, bridge.ErrProtocol
	}
	fields, err = object(fields["outcome"])
	if err != nil || required(fields, "ready", &out.ready) != nil || required(fields, "reused", &out.reused) != nil ||
		nullable(fields, "published", &out.published) != nil || nullable(fields, "media_code", &out.mediaCode) != nil ||
		nullable(fields, "auth_code", &out.authCode) != nil {
		return downloadOutcome{}, bridge.ErrProtocol
	}
	if out.mediaCode != nil && !knownDownloadCode(*out.mediaCode) {
		return downloadOutcome{}, bridge.ErrProtocol
	}
	if out.authCode != nil && *out.authCode != "storage_failed" && *out.authCode != "storage_uncertain" {
		return downloadOutcome{}, bridge.ErrProtocol
	}
	raw, present := fields["chapters"]
	if !present {
		return downloadOutcome{}, bridge.ErrProtocol
	}
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		count := 0
		out.chapters, err = downloadChapters(raw, 1, &count)
		if err != nil || model.ValidateChapters(out.chapters) != nil {
			return downloadOutcome{}, bridge.ErrProtocol
		}
	}
	if (out.reused && (!out.ready || out.published == nil || *out.published)) ||
		(out.published != nil && *out.published && !out.ready) ||
		(out.chapters != nil && (!out.ready || out.reused || out.published == nil || !*out.published)) ||
		(!out.ready && out.mediaCode == nil && out.authCode == nil) {
		return downloadOutcome{}, bridge.ErrProtocol
	}
	return out, nil
}
func downloadChapters(raw []byte, depth int, count *int) ([]model.Chapter, error) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || items == nil || (len(items) > 0 && depth > model.MaxChapterDepth) {
		return nil, bridge.ErrProtocol
	}
	result := make([]model.Chapter, 0, len(items))
	for _, item := range items {
		*count++
		if *count > model.MaxChapters {
			return nil, bridge.ErrProtocol
		}
		fields, err := object(item)
		var node model.Chapter
		if err != nil || required(fields, "id", &node.ID) != nil || required(fields, "title", &node.Title) != nil {
			return nil, bridge.ErrProtocol
		}
		node.Start, err = millisecondsDuration(fields, "start_ms")
		if err != nil {
			return nil, err
		}
		if _, ok := fields["end_ms"]; ok {
			end, err := millisecondsDuration(fields, "end_ms")
			if err != nil {
				return nil, err
			}
			node.End = &end
		}
		node.Children, err = downloadChapters(fields["children"], depth+1, count)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, nil
}
func reconciliationTerminal(frame bridge.Frame) (model.MediaState, error) {
	fields, err := strictFields(frame.Raw)
	var state model.MediaState
	if err != nil || frame.Type != bridge.Result || required(fields, "state", &state) != nil {
		return model.MediaUnknown, bridge.ErrProtocol
	}
	switch state {
	case model.MediaReady, model.MediaAbsent, model.MediaInvalid, model.MediaUnsafe, model.MediaUnknown:
		return state, nil
	default:
		return model.MediaUnknown, bridge.ErrProtocol
	}
}
