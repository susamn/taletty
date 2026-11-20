package audible

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/susamn/taletty/engine/model"
)

func TestLibraryCancellationWithIncomingPages(t *testing.T) {
	client := libraryClient(t)
	var transcript strings.Builder
	for index := 1; index <= 100; index++ {
		book := strings.Replace(bookPayload, "T000000001", fmt.Sprintf("T%09d", index), 1)
		transcript.WriteString(wire("stage", index, `"stage":"library_page","books":[`+book+`]`))
	}
	transcript.WriteString(wire("result", 101, `"count":100`))
	setTranscript(client, transcript.String(), 0, false)
	for range 10 {
		ctx, cancel := context.WithCancel(context.Background())
		var terminal model.EventType
		result, err := client.Library(ctx, "queued-pages", func(e model.OperationEvent) {
			if e.Type == model.Stage {
				cancel()
			}
			terminal = e.Type
		})
		cancel()
		if !errors.Is(err, context.Canceled) || result.Books != nil || terminal != model.Canceled {
			t.Fatal("incoming pages escaped cancellation", err)
		}
	}
}
