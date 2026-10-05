# The README's TUI recording. Run it with `make demo`, not vhs directly:
# record.sh starts the demo daemon this tape talks to, puts a `spore` wrapper
# for it on PATH, and sets SPORE_DEMO_WS.
#
# The Hide/Show pairs cut out the time the model spends working, so the GIF
# shows the request, the approval and the answer, not the waiting.

Output assets/tui-demo.gif
Set Shell bash
Set FontSize 15
Set Width 1280
Set Height 720
Set Padding 16
Set TypingSpeed 35ms
Set WaitTimeout 180s
Set Theme "Catppuccin Mocha"

Hide
Type 'cd "$SPORE_DEMO_WS" && clear'
Enter
Sleep 500ms
Show

Type "spore chat"
Sleep 300ms
Enter
Sleep 2s
Type "Add a .editorconfig that matches this repo's formatting, then tell me what you set."
Sleep 600ms
Enter
Sleep 3s

# The demo policy asks only for file writes, so the first prompt is the
# .editorconfig write.
Hide
Wait+Screen /allow once/
Show
Sleep 3s
Alt+y
Sleep 2s

# The turn footer ends with "$<cost> · <seconds>s" once the turn is done.
Hide
Wait+Screen /out · \$[0-9.]+ · [0-9.]+s/
Show
Sleep 5s
