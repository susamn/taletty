package audible

import "github.com/susamn/taletty/engine/model"

var libraryFailures = map[string]struct{ message, retry string }{
	"not_authenticated":    {"Sign in to Audible first.", "sign_in"},
	"busy":                 {"Operation identifier is already in use.", "user_retry"},
	"invalid_input":        {"Invalid library operation identifier.", "none"},
	"runtime_missing":      {"The Audible runtime is missing.", "none"},
	"runtime_incompatible": {"The provider runtime is incompatible.", "none"},
	"network":              {"Audible could not be reached.", "user_retry"},
	"throttled":            {"Audible requested a pause before retrying.", "wait"},
	"entitlement_denied":   {"Audible did not grant access.", "check_entitlement"},
	"unsupported":          {"Audible marketplace is unsupported.", "none"},
	"protocol_error":       {"Audible library response was invalid.", "none"},
	"canceled":             {"Operation canceled.", "none"},
	"internal":             {"Audible operation failed.", "none"},
	"storage_failed":       {"Authentication storage failed.", "none"},
	"storage_uncertain":    {"Credentials published; durability not confirmed.", "none"},
}

func libraryFailure(code, id string) *model.Failure {
	spec, ok := libraryFailures[code]
	if !ok {
		code = "internal"
		spec = libraryFailures[code]
	}
	return &model.Failure{Code: code, Message: spec.message, Retry: spec.retry, OperationID: id}
}
