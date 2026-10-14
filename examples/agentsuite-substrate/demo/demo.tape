# vhs tape for the recorded demo; record.sh runs it from the repository root. The terminal is
# 1920x1080 at a readable font size, and the frame rate is low because the run is long.
Set Shell "bash"
Set FontSize 17
Set Width 1920
Set Height 1080
Set Padding 12
Set Framerate 8
Set TypingSpeed 25ms
Set Theme "Builtin Dark"

Hide
Type "export PS1='$ ' && clear"
Enter
Show
Type "examples/agentsuite-substrate/demo/record.sh run"
Sleep 1s
Enter
# record.sh run prints the marker on the cursor's line once the driver has returned and its logs are
# fetched. Wait+Screen is not used: in vhs 0.12.1 it stops the terminal drawing streamed output.
# The bound is record.sh's driver deadline plus the setup before it.
Wait+Line@55m /RECORDING (COMPLETE|FAILED)/
Sleep 15s
