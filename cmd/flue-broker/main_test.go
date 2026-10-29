package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestSetProcName verifies that setProcName changes the comm name of the
// calling thread — what `ps` reports for a process, so a broker exec'd
// from the injected memfd (/proc/self/fd/4) still reads as flue-broker.
// It checks /proc/self/task/<tid>/comm of the calling thread, because the
// goroutine may run on a non-main OS thread (whose comm /proc/self/comm
// would not show). PR_SET_NAME is Linux-only, so the test skips
// otherwise.
func TestSetProcName(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("PR_SET_NAME is Linux-only")
	}
	runtime.LockOSThread()
	tid := unix.Gettid()
	comm := func() string {
		b, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/comm", tid))
		if err != nil {
			t.Fatalf("read /proc/self/task/%d/comm: %v", tid, err)
		}
		return strings.TrimSpace(string(b))
	}

	setProcName("flue-broker")
	if got := comm(); got != "flue-broker" {
		t.Fatalf("comm = %q, want flue-broker", got)
	}

	// Names longer than 15 characters are ignored by the guard: prctl
	// would truncate them (dropping the null terminator and leaving a
	// mangled name).
	setProcName("a-very-long-name-more-than-15")
	if got := comm(); got != "flue-broker" {
		t.Fatalf("comm = %q, want flue-broker (overlong name must be ignored)", got)
	}
}

// TestVersionFlag verifies that -version prints the one-line build version
// and exits 0 before any other setup: no agent socket, no control fd, no
// log configuration.
func TestVersionFlag(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"flue-broker", "-version"}

	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("create temp stdout: %v", err)
	}
	oldStdout := os.Stdout
	os.Stdout = f
	code := run()
	os.Stdout = oldStdout
	f.Close()

	if code != 0 {
		t.Fatalf("run() with -version = %d, want 0", code)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	if want := fmt.Sprintf("flue-broker version %s\n", version); string(data) != want {
		t.Errorf("output = %q, want %q", data, want)
	}
}

// TestMarkCloexecAll verifies the invariant that runChild relies on: after
// markCloexecAll, no fd above 2 survives into a wrapped child, so the control
// socketpair and listener are never leaked to processes like bash.
func TestMarkCloexecAll(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	a, b := pair[0], pair[1]
	defer unix.Close(a)
	defer unix.Close(b)

	markCloexecAll()

	for _, fd := range []int{a, b} {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			t.Fatalf("F_GETFD(%d): %v", fd, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("fd %d: CLOEXEC not set after markCloexecAll", fd)
		}
	}
}

// newControlSocketpair creates a SOCK_SEQPACKET socketpair and returns the
// two ends as net.Conn: the "host" end (held by the test) and the "broker"
// end (handed to runChild). Both are closed via t.Cleanup.
func newControlSocketpair(t *testing.T) (host, broker net.Conn) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	h, err := net.FileConn(os.NewFile(uintptr(pair[0]), "test-host"))
	if err != nil {
		t.Fatalf("file conn host: %v", err)
	}
	b, err := net.FileConn(os.NewFile(uintptr(pair[1]), "test-broker"))
	if err != nil {
		t.Fatalf("file conn broker: %v", err)
	}
	t.Cleanup(func() {
		h.Close()
		b.Close()
	})
	return h, b
}

// notifyTestSignals registers the broker's termination signals and returns
// the channel, the same one run() provides when it calls runChild or
// waitForHost; the subscription is stopped in cleanup.
func notifyTestSignals(t *testing.T) <-chan os.Signal {
	t.Helper()
	sigc := notifySignals()
	t.Cleanup(func() { signal.Stop(sigc) })
	return sigc
}

// TestRunChildExitCode verifies that runChild spawns the given command and
// returns its exit status. The control channel is left open (the "host" end
// stays up), so the child-exit path is what ends runChild.
func TestRunChildExitCode(t *testing.T) {
	for _, tc := range []struct {
		script string
		want   int
	}{
		{script: "exit 0", want: 0},
		{script: "exit 7", want: 7},
	} {
		t.Run(tc.script, func(t *testing.T) {
			_, broker := newControlSocketpair(t)
			sockPath := t.TempDir() + "/flue.sock"

			got := runChild([]string{"sh", "-c", tc.script}, sockPath, broker, notifyTestSignals(t))
			if got != tc.want {
				t.Fatalf("runChild: got exit code %d, want %d", got, tc.want)
			}
		})
	}
}

// makeExec writes an executable shell stand-in named name into dir and
// returns its path.
func makeExec(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestValidateShell verifies the $SHELL usability check: a bare name
// resolves in $PATH, a path is used as-is, and a file is rejected when it
// is missing, not a regular file, not executable, or — with a
// non-empty /etc/shells — not listed in it; a missing, empty, or
// comment-only /etc/shells imposes no restriction. It also pins the
// value-shaping rules: the check does no trimming itself (the
// environment reader does), so padded values simply do not resolve;
// inner whitespace is kept (a multi-word value is one program name,
// never a split command); and NUL or invalid UTF-8 bytes make the
// value unusable rather than fatal.
func TestValidateShell(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	exec := makeExec(t, dir, "mysh")
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	shellsFile := filepath.Join(dir, "shells")
	absentShells := filepath.Join(dir, "shells-absent")

	oldShells := shellsPath
	t.Cleanup(func() { shellsPath = oldShells })
	// writeShells points shellsPath at a file with the given content, or at
	// a nonexistent path when content is nil.
	writeShells := func(content *string) {
		if content == nil {
			shellsPath = absentShells
			return
		}
		if err := os.WriteFile(shellsFile, []byte(*content), 0o644); err != nil {
			t.Fatalf("write shells: %v", err)
		}
		shellsPath = shellsFile
	}
	shellsContent := func(s string) *string { return &s }

	for _, tc := range []struct {
		name     string
		s        string
		shells   *string
		wantPath string
		wantWhy  string
	}{
		{"bare name resolves in $PATH", "mysh", nil, exec, ""},
		{"absolute path used as-is", exec, nil, exec, ""},
		{"trailing newline is not trimmed, so the path does not exist", exec + "\n", nil, "", "not found"},
		{"surrounding whitespace is not trimmed, so the name does not resolve", "  mysh  ", nil, "", "not found in $PATH"},
		{"inner newline is kept, so the path does not exist", dir + "/my\nsh", nil, "", "not found"},
		{"NUL byte makes the value unusable", dir + "/nul" + "\x00" + exec, nil, "", "not found"},
		{"invalid UTF-8 byte makes the value unusable", dir + "/\xffmysh", nil, "", "not found"},
		{"missing file", filepath.Join(dir, "nope"), nil, "", "not found"},
		{"not executable", plain, nil, "", "not executable"},
		{"directory is not a regular file", dir, nil, "", "not a regular file"},
		{"listed in /etc/shells", exec, shellsContent(exec + "\n/bin/other\n"), exec, ""},
		{"not listed in /etc/shells", exec, shellsContent("/bin/other\n"), "", "not listed in " + shellsFile},
		{"empty /etc/shells imposes no restriction", exec, shellsContent(""), exec, ""},
		{"comment-only /etc/shells imposes no restriction", exec, shellsContent("# only a comment\n"), exec, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeShells(tc.shells)
			path, why := validateShell(tc.s)
			if path != tc.wantPath || why != tc.wantWhy {
				t.Fatalf("validateShell(%q) = (%q, %q), want (%q, %q)", tc.s, path, why, tc.wantPath, tc.wantWhy)
			}
		})
	}
}

// TestResolveDefaultShell verifies the -default-shell fallback order:
// $SHELL (when usable) first, then the fixed candidates, then the path
// names in $PATH, and a clear error when nothing is usable. The fixed
// candidates and $PATH point at separate temporary directories so the
// host's /bin is never consulted. It also pins the $SHELL edge cases:
// unset, set-but-empty, and whitespace- or newline-padded values are
// trimmed; a value with inner whitespace or a newline is one program
// name (not a command to split), so it simply falls through; garbage
// values never abort the resolution, they only lose the first place.
func TestResolveDefaultShell(t *testing.T) {
	oldCand, oldNames, oldShells := defaultShellCandidates, defaultShellPathNames, shellsPath
	t.Cleanup(func() {
		defaultShellCandidates, defaultShellPathNames, shellsPath = oldCand, oldNames, oldShells
	})

	for _, tc := range []struct {
		name        string
		shellEnv    string
		shellUnset  bool     // $SHELL is not set at all (else shellEnv is set, possibly empty)
		shellFile   bool     // whether the $SHELL file exists in the $PATH dir
		cand        []string // candidate names that exist
		pathNames   []string // other names that exist in the $PATH dir
		want        string
		wantSource  string
		wantErrPart string
	}{
		{
			name:       "$SHELL wins when usable",
			shellEnv:   "mysh",
			shellFile:  true,
			pathNames:  []string{"bash"},
			wantSource: "$SHELL",
		},
		{
			name:       "unset $SHELL falls through to the first candidate",
			shellUnset: true,
			cand:       []string{"bash"},
			wantSource: "cand/bash",
		},
		{
			name:       "empty $SHELL falls through to the first candidate",
			shellEnv:   "",
			cand:       []string{"bash"},
			wantSource: "cand/bash",
		},
		{
			name:       "surrounding whitespace in $SHELL is trimmed",
			shellEnv:   "  mysh  ",
			shellFile:  true,
			wantSource: "$SHELL",
		},
		{
			name:       "trailing newline in $SHELL is trimmed",
			shellEnv:   "mysh\n",
			shellFile:  true,
			wantSource: "$SHELL",
		},
		{
			name:       "$SHELL with an inner space is not split into a command",
			shellEnv:   "mysh -l",
			cand:       []string{"bash"},
			wantSource: "cand/bash",
		},
		{
			name:       "$SHELL with an inner newline is not split into a command",
			shellEnv:   "mysh\nl",
			cand:       []string{"bash"},
			wantSource: "cand/bash",
		},
		{
			name:       "$SHELL missing falls through to the first candidate",
			shellEnv:   "absent",
			cand:       []string{"bash"},
			wantSource: "cand/bash",
		},
		{
			name:       "second candidate when the first is absent",
			cand:       []string{"sh"},
			wantSource: "cand/sh",
		},
		{
			name:       "$PATH bash when no candidate exists",
			pathNames:  []string{"bash", "sh"},
			wantSource: "path/bash",
		},
		{
			name:       "$PATH sh when only sh exists",
			pathNames:  []string{"sh"},
			wantSource: "path/sh",
		},
		{
			name:        "nothing usable errors with the fallback order",
			shellEnv:    "absent",
			wantErrPart: "no usable shell found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candDir, pathDir := t.TempDir(), t.TempDir()
			t.Setenv("PATH", pathDir)
			defaultShellCandidates = nil
			for _, n := range tc.cand {
				defaultShellCandidates = append(defaultShellCandidates, filepath.Join(candDir, n))
			}
			defaultShellPathNames = []string{"bash", "sh"}
			shellsPath = filepath.Join(candDir, "shells-absent")
			for _, n := range tc.cand {
				makeExec(t, candDir, n)
			}
			for _, n := range tc.pathNames {
				makeExec(t, pathDir, n)
			}
			if tc.shellFile {
				// The file exists under the trimmed name: the resolution
				// trims the value before looking it up.
				makeExec(t, pathDir, strings.TrimSpace(tc.shellEnv))
			}
			if tc.shellUnset {
				os.Unsetenv("SHELL")
			} else {
				t.Setenv("SHELL", tc.shellEnv)
			}

			want := ""
			wantSource := tc.wantSource
			switch tc.wantSource {
			case "$SHELL":
				want = filepath.Join(pathDir, strings.TrimSpace(tc.shellEnv))
			case "cand/bash":
				want, wantSource = filepath.Join(candDir, "bash"), filepath.Join(candDir, "bash")
			case "cand/sh":
				want, wantSource = filepath.Join(candDir, "sh"), filepath.Join(candDir, "sh")
			case "path/bash":
				want, wantSource = filepath.Join(pathDir, "bash"), "bash in $PATH"
			case "path/sh":
				want, wantSource = filepath.Join(pathDir, "sh"), "sh in $PATH"
			}

			gotPath, gotSource, err := resolveDefaultShell()
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("resolveDefaultShell() = (%q, %q, nil), want an error", gotPath, gotSource)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDefaultShell() error: %v", err)
			}
			if gotPath != want || gotSource != wantSource {
				t.Fatalf("resolveDefaultShell() = (%q, %q), want (%q, %q)", gotPath, gotSource, want, wantSource)
			}
		})
	}
}

// TestResolveDefaultShellWarnsUnusableSHELL verifies that a set-but-
// unusable $SHELL is logged at warn with the value and the reason (the
// user's choice is being overridden, so the fallback should not be a
// surprise), while an unset or empty $SHELL logs nothing, and that both
// still fall through to the first usable fallback.
func TestResolveDefaultShellWarnsUnusableSHELL(t *testing.T) {
	pathDir := t.TempDir()
	t.Setenv("PATH", pathDir)
	bash := makeExec(t, pathDir, "bash")
	noexec := filepath.Join(pathDir, "noexec")
	if err := os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write noexec: %v", err)
	}

	oldShells := shellsPath
	t.Cleanup(func() { shellsPath = oldShells })
	shellsPath = filepath.Join(pathDir, "shells-absent")

	oldCand := defaultShellCandidates
	t.Cleanup(func() { defaultShellCandidates = oldCand })
	defaultShellCandidates = []string{"/nonexistent/bash"}

	oldLog := log
	t.Cleanup(func() { log = oldLog })
	var buf bytes.Buffer
	log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	for _, tc := range []struct {
		name     string
		shellEnv string
		unset    bool
		wantWarn bool
		wantWhy  string
	}{
		{"unset $SHELL logs nothing", "", true, false, ""},
		{"empty $SHELL logs nothing", "", false, false, ""},
		{"missing bare name warns", "nope", false, true, "not found in $PATH"},
		{"missing path warns", "/nonexistent/xyz", false, true, "not found"},
		{"multi-word value is not a path, warns", "mysh -l", false, true, "not found in $PATH"},
		{"not executable warns", noexec, false, true, "not executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			if tc.unset {
				os.Unsetenv("SHELL")
			} else {
				t.Setenv("SHELL", tc.shellEnv)
			}
			gotPath, gotSource, err := resolveDefaultShell()
			if err != nil {
				t.Fatalf("resolveDefaultShell() error: %v", err)
			}
			if gotPath != bash || gotSource != "bash in $PATH" {
				t.Fatalf("resolveDefaultShell() = (%q, %q), want the $PATH bash", gotPath, gotSource)
			}
			out := buf.String()
			if tc.wantWarn {
				if !strings.Contains(out, "$SHELL is set but not usable") {
					t.Fatalf("want a $SHELL warning in the log, got: %s", out)
				}
				if !strings.Contains(out, tc.shellEnv) {
					t.Fatalf("warning does not name the value %q: %s", tc.shellEnv, out)
				}
				if !strings.Contains(out, tc.wantWhy) {
					t.Fatalf("warning does not name the reason %q: %s", tc.wantWhy, out)
				}
			} else if out != "" {
				t.Fatalf("want no log output, got: %s", out)
			}
		})
	}
}

// resetRunFlags restores the broker's flag variables to their declared
// defaults. The stdlib flag package does not reset values between
// flag.Parse calls in one process, so a run() call from an earlier test
// can leave behind a value this test's arguments do not carry (e.g.
// -version set by TestVersionFlag).
func resetRunFlags() {
	*sockPath = ""
	*fdArg = "3"
	*logLevel = "info"
	*defaultShell = false
	*showVersion = false
}

// TestRunDefaultShellFlag verifies run() end-to-end in -default-shell
// mode: a real control socketpair dup'd onto fd 3, a $SHELL pointing at a
// temporary script that records its run and exits 4, and the child's exit
// code is returned.
func TestRunDefaultShellFlag(t *testing.T) {
	resetRunFlags()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer unix.Close(pair[0])
	defer unix.Close(pair[1])
	if err := unix.Dup2(pair[1], 3); err != nil {
		t.Fatalf("dup2 to fd 3: %v", err)
	}
	defer unix.Close(3)

	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	shell := filepath.Join(dir, "mysh")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\ntouch "+marker+"\nexit 4\n"), 0o755); err != nil {
		t.Fatalf("write shell: %v", err)
	}
	t.Setenv("SHELL", shell)
	// The default /etc/shells (restored once the earlier tests' cleanups
	// run) would reject this /tmp script as an unlisted login shell.
	oldShells := shellsPath
	shellsPath = filepath.Join(dir, "shells-absent")
	t.Cleanup(func() { shellsPath = oldShells })

	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"flue-broker", "-fd", "3", "-sock", filepath.Join(dir, "s.sock"), "-default-shell"}

	if got := run(); got != 4 {
		t.Fatalf("run() = %d, want 4 (the $SHELL child's exit code)", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the $SHELL script did not run: %v", err)
	}
}

// TestRunDefaultShellMutualExclusion verifies that -default-shell and a
// positional command is a startup error, reported before any socket setup
// (no control fd needed).
func TestRunDefaultShellMutualExclusion(t *testing.T) {
	resetRunFlags()
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"flue-broker", "-default-shell", "bash"}

	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("create temp stderr: %v", err)
	}
	defer f.Close()
	oldStderr := os.Stderr
	os.Stderr = f
	code := run()
	os.Stderr = oldStderr
	data, _ := os.ReadFile(f.Name())
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if !strings.Contains(string(data), "cannot be combined with a command") {
		t.Errorf("output %q does not name the conflict", data)
	}
}

func TestWithEnv(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		key  string
		val  string
		want []string
	}{
		{"adds missing", []string{"A=1", "B=2"}, "C", "3", []string{"A=1", "B=2", "C=3"}},
		{"replaces existing", []string{"A=1", "C=old", "B=2"}, "C", "3", []string{"A=1", "B=2", "C=3"}},
		{"keeps longer key", []string{"A=1", "CX=1", "B=2"}, "C", "3", []string{"A=1", "CX=1", "B=2", "C=3"}},
		{"empty env", nil, "SSH_AUTH_SOCK", "/tmp/x.sock", []string{"SSH_AUTH_SOCK=/tmp/x.sock"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withEnv(tc.env, tc.key, tc.val)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("withEnv = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChildExitCode(t *testing.T) {
	if got := childExitCode(nil); got != 0 {
		t.Fatalf("nil err: got %d, want 0", got)
	}
	if got := childExitCode(errors.New("boom")); got != 1 {
		t.Fatalf("non-exit err: got %d, want 1", got)
	}
}

// TestCheckControlFD verifies the pre-handoff validation accepts the
// SOCK_SEQPACKET control socketpair and rejects the failure modes that
// would otherwise surface only deep inside sendListener as an opaque
// "socket operation on non-socket": an fd that is open but not a
// socket (e.g. an exec run without --preserve-fds), and a socket of the
// wrong type.
func TestCheckControlFD(t *testing.T) {
	t.Run("seqpacket socketpair is accepted", func(t *testing.T) {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
		if err != nil {
			t.Fatalf("socketpair: %v", err)
		}
		defer unix.Close(pair[0])
		defer unix.Close(pair[1])
		if err := checkControlFD(pair[0]); err != nil {
			t.Fatalf("checkControlFD(%d) = %v, want nil", pair[0], err)
		}
	})

	t.Run("stream socket is rejected", func(t *testing.T) {
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatalf("socket: %v", err)
		}
		defer unix.Close(fd)
		err = checkControlFD(fd)
		if err == nil {
			t.Fatal("checkControlFD(stream fd) = nil, want an error")
		}
		if !strings.Contains(err.Error(), "SOCK_STREAM") {
			t.Errorf("error %q does not name SOCK_STREAM", err)
		}
	})

	t.Run("dgram socket is rejected", func(t *testing.T) {
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
		if err != nil {
			t.Fatalf("socket: %v", err)
		}
		defer unix.Close(fd)
		err = checkControlFD(fd)
		if err == nil {
			t.Fatal("checkControlFD(dgram fd) = nil, want an error")
		}
		if !strings.Contains(err.Error(), "SOCK_DGRAM") {
			t.Errorf("error %q does not name SOCK_DGRAM", err)
		}
	})

	t.Run("regular file is rejected", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "fd")
		if err != nil {
			t.Fatalf("createtemp: %v", err)
		}
		defer f.Close()
		fd := int(f.Fd())
		err = checkControlFD(fd)
		if err == nil {
			t.Fatal("checkControlFD(file fd) = nil, want an error")
		}
		if !strings.Contains(err.Error(), "not a socket") {
			t.Errorf("error %q does not say the fd is not a socket", err)
		}
	})

	t.Run("closed fd is rejected", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "fd")
		if err != nil {
			t.Fatalf("createtemp: %v", err)
		}
		fd := int(f.Fd())
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := checkControlFD(fd); err == nil {
			t.Fatal("checkControlFD(closed fd) = nil, want an error")
		}
	})
}

// TestRunChildForwardsSIGTERM verifies that SIGTERM delivered to the broker
// reaches the child, and that the child's exit status is still returned
// (so the caller's cleanup runs).
func TestRunChildForwardsSIGTERM(t *testing.T) {
	_, broker := newControlSocketpair(t)
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = unix.Kill(os.Getpid(), unix.SIGTERM)
	}()
	script := `trap 'exit 9' TERM; while :; do sleep 0.05; done`
	if got := runChild([]string{"sh", "-c", script}, t.TempDir()+"/s.sock", broker, notifyTestSignals(t)); got != 9 {
		t.Fatalf("runChild: got exit code %d, want 9", got)
	}
}

// TestWaitForHostSIGTERM verifies that standalone mode returns cleanly on
// SIGTERM instead of dying with the socket left behind.
func TestWaitForHostSIGTERM(t *testing.T) {
	_, broker := newControlSocketpair(t)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = unix.Kill(os.Getpid(), unix.SIGTERM)
	}()
	done := make(chan int, 1)
	go func() { done <- waitForHost(broker, notifyTestSignals(t)) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("waitForHost: got %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForHost did not return after SIGTERM")
	}
}

// TestRunChildSignalExitCode verifies that a child killed by a signal is
// reported shell-style as 128+signal rather than -1/255.
func TestRunChildSignalExitCode(t *testing.T) {
	_, broker := newControlSocketpair(t)
	if got := runChild([]string{"sh", "-c", "kill -9 $$"}, t.TempDir()+"/s.sock", broker, notifyTestSignals(t)); got != 137 {
		t.Fatalf("runChild: got exit code %d, want 137", got)
	}
}
