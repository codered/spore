package main

import (
	"bufio"
	"os"
)

// stdinLines is the process's single reader over standard input. Both the
// chat loop and the approval prompt take lines from it.
var stdinLines = newStdinScanner()

func newStdinScanner() *bufio.Scanner {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return sc
}
