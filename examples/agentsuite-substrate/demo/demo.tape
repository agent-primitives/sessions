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
Wait+Screen@120m /DEMO (COMPLETE|FAILED)/
Sleep 15s
