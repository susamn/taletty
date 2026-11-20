package audible

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync"

	"github.com/susamn/taletty/engine/connectors/audible/internal/auth"
	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
	"golang.org/x/sys/unix"
)

//go:embed internal/bridge/adapter/auth_store.py
var authStoreSource string

//go:embed internal/bridge/adapter/login.py
var browserLoginSource string

//go:embed internal/bridge/adapter/interactive.py
var interactiveSource string

//go:embed internal/bridge/adapter/sdk_session.py
var sdkSessionSource string

var (
	ErrAuthBusy    = auth.ErrBusy
	ErrClosed      = auth.ErrClosed
	ErrTerminal    = bridge.ErrTerminal
	ErrAuthStorage = errors.New("Authentication storage failed.")
)

var loginMessages = map[string]string{
	"canceled": "Sign-in canceled.", "terminal_required": "Sign-in requires an owned terminal.",
	"invalid_input": "Invalid sign-in URL or redirect.", "runtime_missing": "The Audible SDK is missing.",
	"runtime_incompatible": "The sign-in runtime is incompatible.", "login_failed": "Audible sign-in failed.",
	"storage_failed":    "Credentials could not be saved.",
	"storage_uncertain": "Credentials published; durability not confirmed.",
	"internal":          "Audible sign-in failed.",
}

// LoginError contains a fixed adapter code, never provider/SDK text. Retry is an
// explicit user action, not automatic. storage_uncertain means publication occurred.
type LoginError struct{ Code string }

func (e *LoginError) Error() string {
	if message, ok := loginMessages[e.Code]; ok {
		return message
	}
	return "Audible sign-in failed."
}

// Client owns one root lock and authenticated-operation gate for its lifetime.
// Playback and Doctor do not use it. The Python executable is explicitly supplied;
// no runtime installation, legacy discovery or credentials are accepted here.
// Do not copy a Client; its gate, operation IDs and root have shared ownership.
type Client struct {
	session        *auth.Session
	python, script string
	librarySource  string
	operationsMu   sync.Mutex
	operations     map[string]bool
}

// Open requires an existing effective-UID-owned 0700 root. It creates only the
// private lock, not directories or auth. US marketplace only in this extraction.
func Open(ctx context.Context, root, python string) (*Client, error) {
	session, err := auth.Open(ctx, root)
	if err != nil {
		return nil, authError(err)
	}
	return &Client{session: session, python: python, script: loginScript(browserLoginSource), librarySource: libraryScript()}, nil
}

// Close waits for active authenticated work. A canceled Close can be retried.
func (c *Client) Close(ctx context.Context) error { return authError(c.session.Close(ctx)) }

func authError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrAuthBusy) || errors.Is(err, ErrClosed) {
		return err
	}
	return ErrAuthStorage
}

func loginScript(login string) string {
	// Marshal cannot fail for a map of strings; only engine-embedded code is encoded.
	sources, _ := json.Marshal(map[string]string{"doctor": doctorSource, "auth_store": authStoreSource, "login": login, "sdk_session": sdkSessionSource})
	return "import json\nSOURCES=json.loads(" + strconv.Quote(string(sources)) + ")\n" + interactiveSource + "\nmain(SOURCES)\n"
}

// Login borrows an explicitly owned controlling foreground TTY. Suspend all other
// terminal users first; retain the file until return. Browser fallback/paste input
// use only this TTY. No credentials, redirect, raw frames or provider text escape.
// nil means clean exit/control EOF AND a newly published, SDK-validated private
// snapshot, not entitlement/library proof. Cancellation can race publication:
// it does not roll back an already published snapshot or revoke device credentials.
func (c *Client) Login(ctx context.Context, terminal *os.File) error {
	return c.session.WithRoot(ctx, func(ctx context.Context, root *os.File) error {
		before, err := authIdentity(root)
		if err != nil {
			return err
		}
		id := "login-" + rand.Text()
		request := []byte(`{"protocol":1,"operation_id":"` + id + `","action":"login"}`)
		frame, err := bridge.RunTTY(ctx, c.python, []string{"-I", "-B", "-c", c.script}, request, id, root, terminal, nil)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTerminal) {
				return err
			}
			if errors.Is(err, bridge.ErrStart) {
				return ErrRuntimeMissing
			}
			return ErrRuntimeIncompatible
		}
		if err := loginOutcome(frame); err != nil {
			return err
		}
		after, err := authIdentity(root)
		if err != nil {
			return err
		}
		if after == (authFileID{}) || after == before {
			return ErrRuntimeIncompatible
		}
		return nil
	})
}

type authFileID struct{ device, inode uint64 }

func authIdentity(root *os.File) (authFileID, error) {
	var state unix.Stat_t
	err := unix.Fstatat(int(root.Fd()), "auth.json", &state, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return authFileID{}, nil
	}
	if err != nil || state.Mode&unix.S_IFMT != unix.S_IFREG || state.Mode&07777 != 0600 || state.Uid != uint32(os.Geteuid()) || state.Nlink != 1 || state.Size < 1 || state.Size > 4<<20 {
		return authFileID{}, ErrAuthStorage
	}
	return authFileID{uint64(state.Dev), state.Ino}, nil
}

func loginOutcome(frame bridge.Frame) error {
	var fields map[string]json.RawMessage
	if frame.Seq != 1 || json.Unmarshal(frame.Raw, &fields) != nil {
		return ErrRuntimeIncompatible
	}
	if frame.Type == bridge.Result {
		var status string
		if json.Unmarshal(fields["status"], &status) != nil || status != "signed_in" {
			return ErrRuntimeIncompatible
		}
		return nil
	}
	var failure map[string]json.RawMessage
	var code string
	if frame.Type != bridge.Failure || json.Unmarshal(fields["failure"], &failure) != nil || json.Unmarshal(failure["code"], &code) != nil {
		return ErrRuntimeIncompatible
	}
	if _, known := loginMessages[code]; !known {
		return ErrRuntimeIncompatible
	}
	if code == "canceled" {
		return context.Canceled
	}
	return &LoginError{Code: code}
}
