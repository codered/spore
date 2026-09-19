# Decisions taken while clearing the gates

Each entry: the finding, what the code actually does, what I did, and why.

## internal/policy/guard.go — SA4006, value of `claimed` never used

The code at line 254 is:
`if claimed, err := g.store.ResolvePendingCall(book, pendingID, "error"); err == nil && claimed {`

The value of `claimed` is assigned but never used - the condition only checks `err == nil && claimed`, but `claimed` is used in the condition itself. The assignment to `claimed` is actually used in the condition `err == nil && claimed`. Wait, looking more carefully: the variable `claimed` is assigned the result of `ResolvePendingCall`, and then the condition checks `err == nil && claimed`. So `claimed` is used in the condition. But the linter says "this value of `claimed` is never used". Let me look at the code more carefully.

Actually, the linter SA4006 is about a value assigned to a variable that is never used. In `if claimed, err := g.store.ResolvePendingCall(book, pendingID, "error"); err == nil && claimed {`, the variable `claimed` is used in the condition `err == nil && claimed`. So this should be fine. Let me check the actual code.

## internal/subagent/supervisor.go — unused (*Supervisor).track

The function `track(id string, c *child)` at line 196 is defined but never called. The supervisor uses `tryTrack` to track children, which is the function that actually inserts into the `running` map with proper locking and reference counting. The `track` function appears to be leftover from earlier sub-agent work and is not needed.

## internal/recall/weaviate/weaviate.go — SA1019, WithObject deprecated

The code uses `batcher.WithObject(chunkObject(c))` which is deprecated. Changed to `batcher.WithObjects(chunkObject(c))` following the deprecation message.

## internal/trace/trace_test.go — SA1019, Value.Emit deprecated

The code uses `kv.Value.Emit()` which is deprecated. Changed to `kv.Value.String()`.

## cmd/spore/tui.go — unused slashHint field

The field `slashHint string` on line 103 is never used. Deleted the field.

## internal/daemon/e2e_test.go — unused newSession function

The function `newSession` at line 107 is never used. Deleted the function.
