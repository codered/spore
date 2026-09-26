package kernel

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
)

// Message types on the wire. See the spec's protocol table: run and result
// go parent→child; out, call and done go child→parent.
const (
	msgRun    = "run"
	msgOut    = "out"
	msgCall   = "call"
	msgResult = "result"
	msgDone   = "done"
)

// msg is every message in both directions. One JSON object per line;
// encoding/json escapes newlines inside strings, so a line is a message.
type msg struct {
	Type      string          `json:"type"`
	Code      string          `json:"code,omitempty"`
	MaxOutput int             `json:"max_output,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        int64           `json:"id,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Error     string          `json:"error,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

// conn is one direction pair of the pipe. Writes are serialised so that
// concurrent helper calls and output never interleave within a line.
type conn struct {
	r  *bufio.Reader
	mu sync.Mutex
	w  io.Writer
}

func newConn(r io.Reader, w io.Writer) *conn {
	return &conn{r: bufio.NewReader(r), w: w}
}

// read returns the next message. Lines have no length limit: a helper
// result can be megabytes, which bufio.Scanner would refuse.
func (c *conn) read() (msg, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return msg{}, err
	}
	var m msg
	err = json.Unmarshal(line, &m)
	return m, err
}

func (c *conn) write(m msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(b)
	return err
}
