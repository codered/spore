package tui

import (
	"context"
	"time"
)

// Resource is one k9s-style view: what its table shows and what can be done
// to a row. The table component does the rest.
type Resource interface {
	Name() string   // "skills": the :command and the table title
	Hotkey() string // "S"
	Scoped() bool   // true: rows belong to the selected session
	Columns() []Column
	Fetch(ctx context.Context, v Views, sessionID string) ([]Row, error)
	Detail(r Row) string // shown by enter / d
	Actions() []Action
}

// Column is one table column.
type Column struct {
	Title string
	Min   int  // minimum width in cells
	Flex  bool // takes the remaining width; at most one per resource
	Right bool // right-aligned (numbers)
}

// Row is one table row. ID is stable across refreshes, so the cursor can
// follow a row the refresh moved.
type Row struct {
	ID    string
	Cells []string
	Data  any // the typed value, for Detail and Actions
}

// Action is something a key does to the selected row.
type Action struct {
	Key     string
	Label   string
	Applies func(Row) bool   // nil: every row
	Confirm func(Row) string // nil: runs without asking
	Run     func(ctx context.Context, v Views, r Row) error
}

// opener is a Resource whose rows lead to a session: enter selects that
// session and returns to the chat screen instead of opening the detail pane.
type opener interface {
	Open(r Row) (sessionID string)
}

// driller is a Resource whose rows open another view: enter opens it over
// this one, and esc comes back.
type driller interface {
	Drill(r Row) (Resource, bool)
}

// sessionOpener is a Resource whose rows are sessions that o opens in the
// chat, while enter keeps showing the row's detail.
type sessionOpener interface {
	OpenSession(r Row) (sessionID string)
}

// clock is the resources' notion of now, for ages and relative times. Tests
// pin it.
var clock = time.Now
