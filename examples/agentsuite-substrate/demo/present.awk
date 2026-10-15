# Renders driver.sh show's output for the recording. Each line is printed without its newline until
# the next line arrives, so the newest line stays on the cursor's line, where vhs's Wait+Line looks;
# a line starting with "@@R " replaces the line before it. Docker's log stream, which hq remote-test
# relays, only passes whole lines, so the driver cannot hold a partial line itself.
{
	if (substr($0, 1, 4) == "@@R ") {
		printf "\r\033[K%s", substr($0, 5)
	} else {
		if (NR > 1) printf "\n"
		printf "%s", $0
	}
	fflush()
}
END { printf "\n" }
