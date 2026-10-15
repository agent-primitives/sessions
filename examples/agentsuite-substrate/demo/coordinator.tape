# vhs tape for the coordinator demo; coordinator-record.sh record runs it from the repository root.
#
# As demo.tape: the recording starts hidden, the tape types `coordinator-record.sh run` (coordinator.sh
# show on the host through present.awk) and shows the terminal from the title card on. Each wait
# coordinator.sh show runs out of frame (run -w) prints "⠿ ..." and then "✓ ... in N s"; the tape hides
# the recording between the two. The pairs below follow coordinator.sh's `run -w` calls in order;
# scripts_test.go checks the counts match. Every wait also matches a failure marker.
Set Shell "bash"
Set FontSize 22
Set Width 1920
Set Height 1080
Set Padding 36
Set Framerate 10
Set TypingSpeed 5ms
Set Theme "Builtin Dark"
Set CursorBlink false

Hide
Type "export PS1='$ ' && clear && examples/agentsuite-substrate/demo/coordinator-record.sh run"
Enter
# The title card's last line; the job has started and the port-forward answers.
Wait+Line@20m /approval\.|RECORDING FAILED|DEMO FAILED/
Show

# 1. chapter 4: the approved request's turn
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 2. chapter 4: agentctl approve
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 3. chapter 6: the denied request's turn
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 4. chapter 6: agentctl deny
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@5m /✓|RECORDING FAILED|DEMO FAILED/
Show

# 5. chapter 7: go test of the bounds
Wait+Line@5m /⠿|RECORDING FAILED|DEMO FAILED/
Sleep 500ms
Hide
Wait+Line@10m /✓|RECORDING FAILED|DEMO FAILED/
Show

# The last line, held for the end of the video; then wait, hidden, for the logs to be fetched.
Wait+Line@5m /coordinator demo\.|RECORDING FAILED|DEMO FAILED/
Sleep 6s
Hide
Wait+Line@10m /RECORDING (COMPLETE|FAILED)/
