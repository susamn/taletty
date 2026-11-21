package model

// LibrarySnapshot is authoritative only on a successful Library return. Error
// returns have the zero value; a successful empty library has non-nil empty Books.
type LibrarySnapshot struct {
	OperationID string `json:"operation_id"`
	Books       []Book `json:"books"`
}

// Failure is an engine-created safe code/message/retry record, not provider text.
// Consumers may inspect it with errors.As; never construct it from remote prose.
type Failure struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Retry       string `json:"retry"`
	OperationID string `json:"operation_id,omitempty"`
}

func (f Failure) Error() string { return f.Message }

type EventType string

const (
	Started   EventType = "started"
	Stage     EventType = "stage"
	Completed EventType = "completed"
	Failed    EventType = "failed"
	Canceled  EventType = "canceled"
)

// OperationEvent reports ordered state, never a partially usable snapshot. Count
// on library_page is provisional; only Completed/Library's return commits it.
// No Books are shared with callbacks; failure records are independent copies.
type OperationEvent struct {
	OperationID string    `json:"operation_id"`
	Sequence    uint64    `json:"sequence"`
	Type        EventType `json:"type"`
	Stage       string    `json:"stage,omitempty"`
	Count       int       `json:"count"`
	Failure     *Failure  `json:"failure,omitempty"`
	// Download is an owned terminal result, including media facts on failure.
	Download *DownloadResult `json:"download,omitempty"`
}
