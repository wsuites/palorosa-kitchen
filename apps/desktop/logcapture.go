//go:build windows

package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// logRing keeps the most recent log lines in memory so Ajustes can show them,
// and mirrors them to app.log in the data directory once it is known.
type logRing struct {
	mu    sync.Mutex
	lines []string
	file  *os.File
}

const logRingSize = 300

// maxLogFile is the size at which app.log is rotated to app.log.1 on start.
const maxLogFile = 2 << 20

func (b *logRing) add(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	if len(b.lines) > logRingSize {
		b.lines = b.lines[len(b.lines)-logRingSize:]
	}
	if b.file != nil {
		_, _ = b.file.WriteString(line + "\n")
	}
}

// Tail returns the last n lines, oldest first.
func (b *logRing) Tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > len(b.lines) {
		n = len(b.lines)
	}
	return strings.Join(b.lines[len(b.lines)-n:], "\n")
}

// AttachFile starts mirroring lines to path, including the ones already
// captured. A large previous log is rotated to path.1.
func (b *logRing) AttachFile(path string) {
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogFile {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		b.add(stamp("log: " + err.Error()))
		return
	}
	// An unhandled panic writes to stderr, which the windowsgui build throws
	// away: mirror the fatal crash into app.log so a close with no message
	// still leaves the reason behind.
	_ = debug.SetCrashOutput(f, debug.CrashOptions{})
	b.mu.Lock()
	defer b.mu.Unlock()
	b.file = f
	_, _ = f.WriteString(fmt.Sprintf("---- %s ----\n", time.Now().Format(time.RFC3339)))
	for _, line := range b.lines {
		_, _ = f.WriteString(line + "\n")
	}
}

var logs = &logRing{}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stamp(line string) string {
	return time.Now().Format("15:04:05") + " " + line
}

// captureOutput routes os.Stdout, os.Stderr and the log package through a
// pipe into the ring. The windowsgui build has no console, so writing to the
// original handles fails; that is why the ring used to stay empty. Lines are
// still copied to the original stderr when it works (go run from a terminal).
func captureOutput() {
	r, w, err := os.Pipe()
	if err != nil {
		return
	}
	original := os.Stderr
	os.Stdout = w
	os.Stderr = w
	log.SetFlags(0)
	log.SetOutput(w)

	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		consoleOK := original != nil
		for scanner.Scan() {
			raw := scanner.Text()
			if consoleOK {
				if _, err := fmt.Fprintln(original, raw); err != nil {
					consoleOK = false
				}
			}
			line := strings.TrimRight(ansiEscape.ReplaceAllString(raw, ""), " \r")
			if line == "" {
				continue
			}
			logs.add(stamp(line))
		}
	}()
}
