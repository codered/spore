package main

import (
	"bufio"
	"os"
)

// lineSource yields one line of user input at a time. Exactly one goroutine
// may read from a given source — that is the whole point of the type.
type lineSource interface {
	// ReadLine returns the next line and true, or "" and false at EOF.
	ReadLine() (string, bool)
}

// scannerLines reads directly from the shared stdin scanner. Safe only where
// a single goroutine consumes input, as in `once`.
type scannerLines struct{ sc *bufio.Scanner }

func (s scannerLines) ReadLine() (string, bool) {
	if !s.sc.Scan() {
		return "", false
	}
	return s.sc.Text(), true
}

// chanLines reads lines a dedicated reader goroutine has already pulled off
// stdin. chat uses this so its main loop is the only consumer, serving both
// the prompt and any approval that arrives.
type chanLines struct{ ch <-chan string }

func (c chanLines) ReadLine() (string, bool) {
	line, ok := <-c.ch
	return line, ok
}

// stdinLines is the process's single reader over standard input.
var stdinLines = newStdinScanner()

func newStdinScanner() *bufio.Scanner {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return sc
}
