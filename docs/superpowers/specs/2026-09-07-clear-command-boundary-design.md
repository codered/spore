# Clear Command Boundary Design

## Problem

`/clear` sends `summary_through = math.MaxInt32`. New messages have smaller sequence numbers, so the agent removes them from every later provider request. Providers that require a user message then fail with `No user query found in messages.`

The `/context` command also reads the complete transcript. It therefore reports archived messages after `/clear` instead of the live model context.

## Design

Add `POST /api/sessions/{id}/clear`. The daemon will atomically read the current maximum message sequence and set the summary boundary to that value in one store transaction. This operation also repairs sessions whose boundary is already `math.MaxInt32`.

The CLI will call this endpoint instead of choosing a boundary. The daemon remains the only component that interprets transcript sequence state.

Add `summary_through` to the transcript response. `/context` will count only messages with a sequence greater than this boundary. `/usage` will continue to count the complete stored transcript because usage is historical.

## Safety and Tests

- Reject a clear request for an unknown session.
- Do not accept a client-supplied sequence in the clear operation.
- Test that clear records the current last sequence.
- Test that a new user turn after clear reaches the provider.
- Test that a prior oversized boundary is repaired.
- Test that `/context` reports only live messages.
