package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/terminal"
	"golang.org/x/sys/unix"
)

// The side channel acknowledges actual model transitions, never screen timing.
// Only test code understands this protocol; production has no runtime test hook.
type terminalControl struct{ Kind, Text string }
type terminalState struct {
	Kind, Key, Selection, Draft, Error    string
	Width, Height                         int
	Focus                                 Pane
	Note, Confirm, Pending, Warning, Help bool
	Actions                               []Action
}
type terminalFixture struct {
	Model
	control <-chan terminalControl
	ctx     context.Context
	report  *json.Encoder
	actions *[]Action
}

func (f terminalFixture) read() tea.Msg {
	select {
	case c := <-f.control:
		return c
	case <-f.ctx.Done():
		return nil
	}
}
func (f terminalFixture) Init() tea.Cmd { return f.read }
func (f terminalFixture) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var next tea.Cmd
	kind, key := "", ""
	if c, ok := msg.(terminalControl); ok {
		kind = c.Kind
		next = f.read
		switch c.Kind {
		case "open":
			msg = OpenNoteEditorMsg{ID: "note", Target: NoteTarget{Key: "a", Title: "Frozen A"}}
		case "confirm":
			msg = OpenConfirmationMsg{ID: "child", Title: "Delete", Keep: "Keep", Confirm: "Delete", Action: Action{Kind: "delete", Key: "a"}}
		case "failure":
			msg = ModalResultMsg{ID: "note", Attempt: 1, Error: "Save failed"}
		case "stale":
			msg = ModalResultMsg{ID: "old", Attempt: 2}
		case "success":
			msg = ModalResultMsg{ID: "note", Attempt: 2}
		case "snapshot":
			msg = SnapshotMsg{Books: []Book{{Key: "b", Title: "B"}}, Player: Player{Key: "b", Session: "new", State: Paused}}
		}
	}
	if k, ok := msg.(tea.KeyPressMsg); ok {
		kind, key = "key", k.String()
	}
	if _, ok := msg.(NoticeMsg); ok {
		kind = "notice"
	}
	if _, ok := msg.(tea.PasteMsg); ok {
		kind = "paste"
	}
	if _, ok := msg.(tea.WindowSizeMsg); ok {
		kind = "resize"
	}
	m, cmd := f.Model.Update(msg)
	f.Model = m.(Model)
	if kind != "" {
		s := terminalState{Kind: kind, Key: key, Selection: f.selected, Focus: f.focus, Width: f.layout.Screen.Width, Height: f.layout.Screen.Height, Help: f.help, Note: f.note != nil, Confirm: f.confirmation != nil, Warning: f.quitWarning, Actions: *f.actions}
		s.Error = f.failure
		if f.note != nil {
			s.Draft = f.note.field.Value()
			s.Pending = f.note.pending
			s.Error = f.note.error
		}
		if err := f.report.Encode(s); err != nil {
			return f, tea.Quit
		}
	}
	return f, tea.Batch(cmd, next)
}

func TestControllingTerminalJourney(t *testing.T) {
	for _, mode := range []string{"journey", "discard", "cancel", "interrupt", "writer"} {
		t.Run(mode, func(t *testing.T) {
			master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
				t.Fatal(err)
			}
			n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
			if err != nil {
				t.Fatal(err)
			}
			slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer slave.Close()
			resize := func(w, h int) {
				t.Helper()
				if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(w), Row: uint16(h)}); err != nil {
					t.Fatal(err)
				}
			}
			resize(144, 42)
			sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			parent := os.NewFile(uintptr(sockets[0]), "parent")
			child := os.NewFile(uintptr(sockets[1]), "child")
			conn, err := net.FileConn(parent)
			parent.Close()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(12 * time.Second))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTerminalHost$")
			cmd.Env = append(os.Environ(), "TALETTY_TERMINAL_FIXTURE="+mode, "GORACE=atexit_sleep_ms=0", "TERM=xterm-256color")
			cmd.ExtraFiles = []*os.File{slave, child}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 3}
			output, err := os.CreateTemp(t.TempDir(), "terminal-host.log")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			readOutput := func() string { data, _ := os.ReadFile(output.Name()); return string(data) }
			cmd.Stdout = output
			cmd.Stderr = output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			child.Close()
			defer func() { cmd.Process.Kill(); cmd.Wait() }()
			drained := make(chan struct{})
			go func() { io.Copy(io.Discard, master); close(drained) }()
			defer func() {
				cmd.Process.Kill()
				cmd.Wait()
				slave.Close()
				master.Close()
				<-drained
			}()
			decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
			await := func(kind string) terminalState {
				t.Helper()
				for {
					var s terminalState
					if err := decoder.Decode(&s); err != nil {
						t.Fatalf("await %s: %v (%s)", kind, err, readOutput())
					}
					if s.Kind == kind {
						return s
					}
				}
			}
			control := func(kind string) terminalState {
				t.Helper()
				if err := encoder.Encode(terminalControl{Kind: kind}); err != nil {
					t.Fatal(err)
				}
				return await(kind)
			}
			key := func(bytes string) terminalState {
				t.Helper()
				if _, err := master.WriteString(bytes); err != nil {
					t.Fatal(err)
				}
				return await("key")
			}
			s := await("resize")
			if s.Width != 144 || s.Height != 42 {
				t.Fatal(s)
			}
			if _, err := terminal.Run(context.Background(), NewModel(SnapshotMsg{}, nil), slave, nil); !errors.Is(err, terminal.ErrTerminal) {
				t.Fatal("background terminal accepted", err)
			}
			if mode == "journey" {
				s = key("j")
				if s.Selection != "b" {
					t.Fatal(s)
				}
				s = key(" ")
				if len(s.Actions) != 1 || s.Actions[0].Key != "a" || s.Actions[0].Session != "loaded" {
					t.Fatal(s)
				}
				key("5")
				key("\t")
				s = key("\x1b[Z")
				if s.Focus != PlayerPane {
					t.Fatal(s)
				}
				key("?")
				s = key("q")
				if !s.Help {
					t.Fatal(s)
				}
				key("\x1b")
				control("open")
				for _, k := range []string{"q", " ", "5", "\x03"} {
					s = key(k)
				}
				if s.Draft != "q 5" || s.Focus != PlayerPane || len(s.Actions) != 1 {
					t.Fatal(s)
				}
				s = key("\x1b\r")
				if s.Draft != "q 5\n" {
					t.Fatal(s)
				}
				master.WriteString("\x1b[200~pasted q 1\nnext\x1b[201~")
				s = await("paste")
				if !strings.Contains(s.Draft, "pasted q 1\nnext") {
					t.Fatal(s)
				}
				draft := s.Draft
				control("snapshot")
				control("confirm")
				s = key("q")
				if !s.Confirm {
					t.Fatal(s)
				}
				s = key("\x1b")
				if s.Confirm || !s.Note || s.Draft != draft {
					t.Fatal(s)
				}
				s = key("\r")
				if !s.Pending || len(s.Actions) != 2 || s.Actions[1].Target.Key != "a" || s.Actions[1].Text != draft {
					t.Fatal(s)
				}
				s = control("failure")
				if s.Pending || s.Error != "Save failed" || s.Draft != draft {
					t.Fatal(s)
				}
				resize(79, 19)
				cmd.Process.Signal(syscall.SIGWINCH)
				s = await("resize")
				if s.Width != 79 {
					t.Fatal(s)
				}
				s = key("q")
				if !s.Warning {
					t.Fatal(s)
				}
				s = key("\x03")
				if !s.Warning || len(s.Actions) != 2 {
					t.Fatal(s)
				}
				s = key("\x1b")
				if s.Warning || s.Draft != draft {
					t.Fatal(s)
				}
				resize(80, 20)
				cmd.Process.Signal(syscall.SIGWINCH)
				s = await("resize")
				if s.Width != 80 || s.Draft != draft {
					t.Fatal(s)
				}
				s = key("\x13")
				if !s.Pending || s.Actions[2].Attempt != 2 {
					t.Fatal(s)
				}
				s = control("stale")
				if !s.Pending {
					t.Fatal(s)
				}
				s = control("success")
				if s.Note {
					t.Fatal(s)
				}
				key("q")
				if s := await("notice"); s.Error != "Shutdown failed; retry" {
					t.Fatal(s)
				}
				key("?")
				key("\x1b")
				key("q")
			} else if mode == "discard" {
				control("open")
				key("q")
				resize(1, 1)
				cmd.Process.Signal(syscall.SIGWINCH)
				await("resize")
				if s := key("q"); !s.Warning || len(s.Actions) != 0 {
					t.Fatal(s)
				}
				if s := key("q"); len(s.Actions) != 1 || s.Actions[0].Kind != RequestQuit {
					t.Fatal(s)
				}
			} else if mode == "interrupt" {
				cmd.Process.Signal(os.Interrupt)
			} else if mode == "cancel" {
				control("cancel")
			} else {
				control("writer")
			}
			await("done")
			if err := cmd.Wait(); err != nil {
				t.Fatal(err, readOutput())
			}
		})
	}
}

type failingTerminalWriter struct{}

func (failingTerminalWriter) Write([]byte) (int, error) {
	return 0, errors.New("fixture writer failure")
}

func TestTerminalHost(t *testing.T) {
	mode := os.Getenv("TALETTY_TERMINAL_FIXTURE")
	if mode == "" {
		return
	}
	tty := os.NewFile(3, "controlling-terminal")
	defer tty.Close()
	socket := os.NewFile(4, "control")
	defer socket.Close()
	saved, err := unix.IoctlGetTermios(3, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	group, err := unix.IoctlGetInt(3, unix.TIOCGPGRP)
	if err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(3, unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	signalContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalContext, 10*time.Second)
	defer cancel()
	var actions []Action
	quitting := false
	quitAttempts := 0
	model := NewModel(SnapshotMsg{Books: []Book{{Key: "a", Title: "A"}, {Key: "b", Title: "B"}}, Player: Player{Key: "a", Session: "loaded", State: Paused}}, func(a Action) tea.Cmd {
		actions = append(actions, a)
		if a.Kind == RequestQuit {
			quitAttempts++
			if mode == "journey" && quitAttempts == 1 {
				return func() tea.Msg { return NoticeMsg{Error: "Shutdown failed; retry"} }
			}
			quitting = true
			return tea.Quit
		}
		return nil
	})
	controls := make(chan terminalControl)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		decoder := json.NewDecoder(socket)
		for {
			var c terminalControl
			if decoder.Decode(&c) != nil {
				return
			}
			select {
			case controls <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() {
		cancel()
		unix.Shutdown(4, unix.SHUT_RDWR)
		<-readerDone
	}()
	fixture := terminalFixture{Model: model, control: controls, ctx: ctx, report: json.NewEncoder(socket), actions: &actions}
	// Cancellation is synchronized through the same typed input boundary. A
	// canceled context joins the program; no sleeps or output matching required.
	wrapped := terminalCancelFixture{terminalFixture: fixture, cancel: cancel, mode: mode}
	var writer io.Writer
	if mode == "writer" {
		writer = failingTerminalWriter{}
	}
	_, err = terminal.Run(ctx, wrapped, tty, writer)
	if (mode == "cancel" || mode == "interrupt") && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if mode == "writer" && err == nil {
		t.Fatal("writer failure lost")
	}
	if (mode == "journey" || mode == "discard") && (err != nil || !quitting) {
		t.Fatal(err)
	}
	actual, e := unix.IoctlGetTermios(3, unix.TCGETS)
	foreground, g := unix.IoctlGetInt(3, unix.TIOCGPGRP)
	actualFlags, flagErr := unix.FcntlInt(3, unix.F_GETFL, 0)
	if e != nil || g != nil || flagErr != nil || actualFlags != flags || *actual != *saved || foreground != group {
		t.Fatal("terminal not restored", e, g)
	}
	json.NewEncoder(socket).Encode(terminalState{Kind: "done"})
}

type terminalCancelFixture struct {
	terminalFixture
	cancel context.CancelFunc
	mode   string
}

func (f terminalCancelFixture) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if c, ok := msg.(terminalControl); ok && (c.Kind == "cancel" || c.Kind == "writer") {
		f.report.Encode(terminalState{Kind: c.Kind})
		if c.Kind == "cancel" {
			f.cancel()
			return f, nil
		}
		return f, tea.Quit
	}
	m, cmd := f.terminalFixture.Update(msg)
	f.terminalFixture = m.(terminalFixture)
	return f, cmd
}
