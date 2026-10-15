# vhs tape for the short demo; record.sh record runs it from the repository root.
#
# The recording starts hidden: the tape types `record.sh run`, which runs driver.sh show (on the host
# with the cluster) through present.awk, and shows the terminal from the title card on. Each wait
# driver.sh runs out of frame (run -w) prints "⠿ ..." and then "✓ ... in N s"; the tape hides the
# recording between the two. The pairs below follow driver.sh's `run -w` calls in order;
# scripts_test.go checks the counts match. Every wait also matches a failure marker, so a failed run
# plays through to its end instead of hanging the tape.
#
# Wait+Line matches the cursor's line, which present.awk keeps on the newest line; Wait+Screen is not
# used, since in vhs 0.12.1 it stops the terminal drawing streamed output.
Set Shell "bash"
Set FontSize 26
Set Width 1920
Set Height 1080
Set Padding 36
Set Framerate 10
Set TypingSpeed 5ms
Set Theme "Builtin Dark"
Set CursorBlink false

Hide
Type "export PS1='$ ' && clear && examples/agentsuite-substrate/demo/record.sh run"
Enter
# The title card's last line; setup is done, so this is the job start and the port-forwards.
Wait+Line@20m /OpenAI\.|RECORDING FAILED|DEMO FAILED/
Show

# 1. chapter 3 How a turn flows: the traced turn; the session's micro-VM starts, the model answers
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 2. chapter 4 Chat: turn 2
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 3. chapter 6 Pause and resume: turn 1 of the memory session
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 4. chapter 6 Pause and resume: suspend
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 5. chapter 6 Pause and resume: sizing the snapshot
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 6. chapter 6 Pause and resume: resume
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 7. chapter 6 Pause and resume: the turn after resume
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 8. chapter 7 Replay: replay
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 9. chapter 8 Fork: fork
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 10. chapter 8 Fork: the child's turn
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 11. chapter 9 Guarding memory: deleting the actor
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 12. chapter 9 Guarding memory: the refused turn
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# The recap's last line, held for the end of the video; then wait, hidden, for the logs to be fetched.
Wait+Line@5m /agentsuite-substrate\.|RECORDING FAILED|DEMO FAILED/
Sleep 7s
Hide
Wait+Line@10m /RECORDING (COMPLETE|FAILED)/
