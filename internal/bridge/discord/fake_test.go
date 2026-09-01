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
	opens    int
	isClosed bool
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

// cloneMessage detaches m's nested slices so the fake owns its own copy.
// Cloning on the way IN rather than on the way out is what makes this safe:
// a caller that reuses its Embeds buffer across streamed edits — which is
// exactly how the renderer works — would otherwise mutate, without
// synchronisation, a slice a test goroutine is reading through sentTo or
// lastEdit.
func cloneMessage(m Message) Message {
	m.Embeds = append([]Embed(nil), m.Embeds...)
	m.Buttons = append([]Button(nil), m.Buttons...)
	return m
}

func (f *fakeClient) Open(ctx context.Context, onMessage func(Inbound), onInteraction func(Interaction)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Counted even on failure: Task 10's supervisor test counts failed
	// connect attempts as attempts, not just successful ones.
	f.opens++
	if err := f.take("Open"); err != nil {
		return err
	}
	f.onMessage, f.onInteraction = onMessage, onInteraction
	return nil
}

func (f *fakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("Close"); err != nil {
		return err
	}
	f.isClosed = true
	return nil
}

func (f *fakeClient) Send(ctx context.Context, channelID string, m Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("Send"); err != nil {
		return "", err
	}
	f.nextID++
	id := fmt.Sprintf("m%d", f.nextID)
	f.sent = append(f.sent, sentMessage{ChannelID: channelID, MessageID: id, Message: cloneMessage(m)})
	return id, nil
}

func (f *fakeClient) Edit(ctx context.Context, channelID, messageID string, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("Edit"); err != nil {
		return err
	}
	f.edits = append(f.edits, sentMessage{ChannelID: channelID, MessageID: messageID, Message: cloneMessage(m)})
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
// because the render goroutine writes to it concurrently. Safe because Send
// clones the nested Embeds/Buttons slices before storing them.
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

// allSent returns a copy of every Send call made so far, in call order,
// regardless of channel.
func (f *fakeClient) allSent() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMessage, len(f.sent))
	copy(out, f.sent)
	return out
}

// editsTo returns a copy of every Edit call for channelID, in call order.
func (f *fakeClient) editsTo(channelID string) []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentMessage
	for _, e := range f.edits {
		if e.ChannelID == channelID {
			out = append(out, e)
		}
	}
	return out
}

// lastEdit returns the most recent Edit call for channelID, and whether one
// exists. Safe to hand to a test directly because Edit clones the nested
// Embeds/Buttons slices before storing them — the returned value shares no
// backing array with anything a writer still holds.
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

// finalContents returns, for each message id Send has sent to channelID, in
// send order, the Content of its most recent Edit if any, otherwise the
// Content it was sent with. This is how a test reads "what the user finally
// sees" after a streamed turn that edits one message repeatedly.
func (f *fakeClient) finalContents(channelID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var ids []string
	sentContent := map[string]string{}
	for _, s := range f.sent {
		if s.ChannelID == channelID {
			ids = append(ids, s.MessageID)
			sentContent[s.MessageID] = s.Message.Content
		}
	}
	latestEdit := map[string]string{}
	for _, e := range f.edits {
		if e.ChannelID == channelID {
			latestEdit[e.MessageID] = e.Message.Content
		}
	}

	out := make([]string, len(ids))
	for i, id := range ids {
		if c, ok := latestEdit[id]; ok {
			out[i] = c
		} else {
			out[i] = sentContent[id]
		}
	}
	return out
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

// openCount returns how many times Open has been called, including calls
// that failed via failNext — Task 10's supervisor retries a failed connect,
// and its test counts attempts, not just successes.
func (f *fakeClient) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

// closed reports whether Close has been called (and did not itself fail via
// failNext).
func (f *fakeClient) closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isClosed
}
