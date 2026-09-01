package discord

import (
	"context"
	"fmt"
	"sync"
)

// fakeClient records what the bridge asked Discord to do and lets a test
// deliver gateway events by hand. Every test in this package uses it; there
// is no other Client implementation under test.
//
// Later tasks' render goroutine writes to this fake from its own goroutine
// while a test's assertions read from the main goroutine, so every field is
// guarded by mu and every accessor returns a copy — never a slice or struct
// that aliases fake state.
type fakeClient struct {
	mu sync.Mutex

	sent     []sentMessage
	edits    []sentMessage
	threads  []createdThread
	responds []respondCall
	nextID   int

	onMessage     func(Inbound)
	onInteraction func(Interaction)

	// failNext makes the next call of the named method return an error, so
	// tests can exercise the bridge's error paths. Keyed by method name
	// ("Open", "Send", "Edit", "CreateThread", "Respond"); consumed once.
	failNext map[string]error
}

type sentMessage struct {
	ChannelID string
	MessageID string
	Message   Message
}

type createdThread struct{ ChannelID, MessageID, Name, ThreadID string }
type respondCall struct{ InteractionID, Token, Content string }

func newFakeClient() *fakeClient { return &fakeClient{failNext: map[string]error{}} }

// take returns and clears the queued failure for name, if any. Callers must
// hold mu.
func (f *fakeClient) take(name string) error {
	if err, ok := f.failNext[name]; ok {
		delete(f.failNext, name)
		return err
	}
	return nil
}

// setFailNext queues an error for the next call of the named method.
func (f *fakeClient) setFailNext(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[name] = err
}

func (f *fakeClient) Open(ctx context.Context, onMessage func(Inbound), onInteraction func(Interaction)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("Open"); err != nil {
		return err
	}
	f.onMessage, f.onInteraction = onMessage, onInteraction
	return nil
}

func (f *fakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.take("Close")
}

func (f *fakeClient) Send(ctx context.Context, channelID string, m Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("Send"); err != nil {
		return "", err
	}
	f.nextID++
	id := fmt.Sprintf("m%d", f.nextID)
	f.sent = append(f.sent, sentMessage{ChannelID: channelID, MessageID: id, Message: m})
	return id, nil
}

func (f *fakeClient) Edit(ctx context.Context, channelID, messageID string, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("Edit"); err != nil {
		return err
	}
	f.edits = append(f.edits, sentMessage{ChannelID: channelID, MessageID: messageID, Message: m})
	return nil
}

func (f *fakeClient) CreateThread(ctx context.Context, channelID, messageID, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("CreateThread"); err != nil {
		return "", err
	}
	f.nextID++
	id := fmt.Sprintf("m%d", f.nextID)
	f.threads = append(f.threads, createdThread{ChannelID: channelID, MessageID: messageID, Name: name, ThreadID: id})
	return id, nil
}

func (f *fakeClient) Respond(ctx context.Context, interactionID, token, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("Respond"); err != nil {
		return err
	}
	f.responds = append(f.responds, respondCall{InteractionID: interactionID, Token: token, Content: content})
	return nil
}

// deliver feeds one gateway message to the bridge, as the real gateway would.
func (f *fakeClient) deliver(in Inbound) {
	f.mu.Lock()
	h := f.onMessage
	f.mu.Unlock()
	if h != nil {
		h(in)
	}
}

// press feeds one button press to the bridge.
func (f *fakeClient) press(i Interaction) {
	f.mu.Lock()
	h := f.onInteraction
	f.mu.Unlock()
	if h != nil {
		h(i)
	}
}

// sentTo returns a copy of every message Send has sent to channelID, in
// call order. Tests must use this rather than reading f.sent directly,
// because the render goroutine writes to it concurrently.
func (f *fakeClient) sentTo(channelID string) []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentMessage
	for _, s := range f.sent {
		if s.ChannelID == channelID {
			out = append(out, s)
		}
	}
	return out
}

// lastEdit returns a copy of the most recent Edit call for channelID, and
// whether one exists.
func (f *fakeClient) lastEdit(channelID string) (sentMessage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.edits) - 1; i >= 0; i-- {
		if f.edits[i].ChannelID == channelID {
			return f.edits[i], true
		}
	}
	return sentMessage{}, false
}

// allThreads returns a copy of every thread CreateThread has created.
func (f *fakeClient) allThreads() []createdThread {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]createdThread, len(f.threads))
	copy(out, f.threads)
	return out
}

// allResponds returns a copy of every Respond call made so far.
func (f *fakeClient) allResponds() []respondCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]respondCall, len(f.responds))
	copy(out, f.responds)
	return out
}
