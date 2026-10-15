# shellcheck shell=bash
# Sourced by the recorded demos' drivers (driver.sh, coordinator.sh show): the screen helpers. The
# driver defines CHAPTERS, COLS, ROWS, READ, LOGS, WORK, STEP_LOG, redact, log and fail first.
C=$'\e[1;36m' Y=$'\e[1;33m' G=$'\e[32m' W=$'\e[1;37m' D=$'\e[2m' N=$'\e[0m'
MAX_LINES=5
# TITLE_HOLD is the drivers' (their title cards).
# shellcheck disable=SC2034
CAPTION_HOLD=2.5 TITLE_HOLD=5 DIAGRAM_HOLD=9 FILE_HOLD=6.5
HEADER_ROWS=4
# wrap TEXT [PREFIX [INDENT]]: TEXT folded to the screen at word boundaries, each line after the
# first indented by INDENT spaces (default the width of PREFIX).
wrap() {
	python3 -c '
import sys, textwrap
text, prefix, cols = sys.argv[1], sys.argv[2], int(sys.argv[3])
indent = " " * (int(sys.argv[4]) if sys.argv[4] else len(prefix))
lines = []
for para in text.split("\n"):
    lines += textwrap.wrap(para, cols - len(prefix) - 2, break_on_hyphens=False, drop_whitespace=False) or [""]
print("\n".join((prefix if i == 0 else indent) + l.rstrip() for i, l in enumerate(lines)))' "$1" "${2:-}" "${COLS}" "${3:-}"
}
# header CAPTION [HOLD]: a fresh screen for the current chapter, with CAPTION, held HOLD seconds
# (default CAPTION_HOLD) before anything else is drawn.
header() {
	[[ "$1" =~ ^[[:print:]]+$ ]] || fail "a caption is not one line of printable characters: $1"
	((${#1} <= COLS - 2)) || fail "a caption is ${#1} characters, wider than the screen's $((COLS - 2)): $1"
	SCREEN_LINES=0 SCREEN_ROWS=${HEADER_ROWS}
	local fill=$((COLS * CH / CHAPTERS))
	printf '\e[2J\e[3J\e[H\e[48;2;0;175;215m%*s\e[48;2;58;58;58m%*s\e[0m\n' "${fill}" '' $((COLS - fill)) ''
	printf '\n%s %d/%d  %s%s\n' "${C}" "${CH}" "${CHAPTERS}" "${TITLE}" "${N}"
	printf '%s%s%s\n' "${W}" "$1" "${N}"
	CAPTION="$1"
	log "" "## screen: $1"
	sleep "${2:-${CAPTION_HOLD}}"
}
chapter() { # n title caption [hold]
	CH=$1 TITLE=$2
	STEP_LOG="${LOGS}/ch-$1.log"
	printf '%s\t%s\t%s\n' "$1" "$(date -u +%s)" "$2" >>"${LOGS}/timeline.tsv"
	printf '# CHAPTER %s  %s  (%s)\n' "$1" "$2" "$(date -u +%FT%TZ)" >"${STEP_LOG}"
	header "$3" "${4:-}"
}
page() { header "$@"; } # caption [hold]: another screen of the current chapter
# show_lines TEXT [MAX]: output rows on screen, two spaces in, then held long enough to read.
show_lines() {
	local text n max="${2:-${MAX_LINES}}"
	text="$(wrap "$1" "  ")"
	n="$(printf '%s\n' "${text}" | wc -l)"
	SCREEN_LINES=$((SCREEN_LINES + n)) SCREEN_ROWS=$((SCREEN_ROWS + n))
	((SCREEN_LINES <= max)) || fail "this screen would show ${SCREEN_LINES} output rows, more than ${max}"
	((SCREEN_ROWS <= ROWS)) || fail "this screen would need ${SCREEN_ROWS} rows, more than the terminal's ${ROWS}"
	printf '%s\n' "${text}"
	log "(on screen: ${n} rows, ${SCREEN_LINES} on this screen, limit ${max})"
	sleep "${READ}"
}
# show_file TEXT CMDROWS: a file screen. TEXT (bat or split-view.py output, colors kept) is shown
# whole; when it is taller than the rows left, it is shown a page at a time, each page a fresh copy
# of the screen (caption and command again) with the next rows, as a pager scrolls.
show_file() {
	local text="$1" cmd_shown="$2" free total start=1 page_no=0
	free=$((ROWS - SCREEN_ROWS))
	total="$(printf '%s\n' "${text}" | wc -l)"
	((free >= 5)) || fail "a file screen with ${free} rows left"
	while ((start <= total)); do
		if ((page_no > 0)); then
			header "${CAPTION}" 0
			printf '%s\n' "${cmd_shown}"
		fi
		printf '%s\n' "${text}" | sed -n "${start},$((start + free - 1))p"
		log "(on screen: rows ${start}-$((start + free - 1 < total ? start + free - 1 : total)) of ${total})"
		sleep "${FILE_HOLD}"
		start=$((start + free)) page_no=$((page_no + 1))
	done
	if ((page_no > 1)); then SCREEN_ROWS=${ROWS}; else SCREEN_ROWS=$((SCREEN_ROWS + total)); fi
}
# run [-w WHAT DONE] [-x] [-f] [-n MAX] <<CMD: the command on the first line of stdin. -w runs it out
# of frame: it prints "⠿ WHAT …" and, once the command is done, replaces that with "✓ DONE in N s";
# the tape hides the recording between the two. -x: the command must fail. -f: a file screen (see
# show_file); the command sees LINES and COLUMNS set to the rows and columns left for its output.
# -n MAX: this screen may show up to MAX output rows.
RAW="${WORK}/run.out" RAW_ERR="${WORK}/run.err"
run() {
	local what="" done="" expect_fail=false file=false max="${MAX_LINES}"
	while (($#)); do
		case "$1" in
		-w) what="$2" done="$3" && shift 3 ;;
		-x) expect_fail=true && shift ;;
		-f) file=true && shift ;;
		-n) max="$2" && shift 2 ;;
		*) fail "run: unknown option $1" ;;
		esac
	done
	local cmdline="" rc=0 t0 took var shown rows
	IFS= read -r cmdline || [[ -n "${cmdline}" ]] || fail "run: no command"
	# As a terminal shows a long command line: cut at the screen's width, not at words.
	shown="$(python3 -c 'import sys; s, w = sys.argv[1], int(sys.argv[2]); print("\n".join(s[i:i + w] for i in range(0, len(s), w)))' "\$ ${cmdline}" "${COLS}")"
	shown="$(printf '%s\n' "${shown}" | sed "s/^/${Y}/; s/\$/${N}/")"
	rows="$(printf '%s\n' "${shown}" | wc -l)"
	printf '%s\n' "${shown}"
	SCREEN_ROWS=$((SCREEN_ROWS + rows))
	log "\$ ${cmdline}"
	sleep "$((rows > 1 ? 1 : 0)).$((rows > 1 ? 4 : 8))"
	if [[ -n "${what}" ]]; then
		printf '  %s⠿ %s …%s\n' "${D}" "${what}" "${N}"
		# The wait's status line takes a row, but it is the driver's, not the command's output, so it
		# does not count toward the screen's output rows.
		SCREEN_ROWS=$((SCREEN_ROWS + 1))
		sleep 1
	fi
	t0=${SECONDS}
	if ${file}; then
		LINES=$((ROWS - SCREEN_ROWS)) COLUMNS=${COLS} eval "${cmdline}" >"${RAW}" 2>"${RAW_ERR}" || rc=$?
	else
		COLUMNS=${COLS} eval "${cmdline}" >"${RAW}" 2>"${RAW_ERR}" || rc=$?
	fi
	took=$((SECONDS - t0))
	redact <"${RAW}" >>"${STEP_LOG}"
	if [[ -s "${RAW_ERR}" ]]; then
		log "(stderr:)"
		redact <"${RAW_ERR}" >>"${STEP_LOG}"
	fi
	if [[ "${cmdline}" =~ ^([A-Z_]+)=\$\( ]]; then
		var="${BASH_REMATCH[1]}"
		log "(${var}=${!var})"
	fi
	log "(exit ${rc} after ${took} s)"
	if [[ -n "${what}" ]]; then
		printf '@@R   %s✓ %s in %d s%s\n' "${G}" "${done}" "${took}" "${N}"
		sleep 1.5
	fi
	if ${expect_fail}; then
		((rc != 0)) || fail "this command was to fail and did not: ${cmdline}"
	else
		((rc == 0)) || fail "exit ${rc}: ${cmdline}"
	fi
	[[ ! -s "${RAW_ERR}" ]] || fail "this command wrote to stderr, which the screen does not show: ${cmdline}"
	SHOWN="$(cat "${RAW}")"
	if [[ -z "${SHOWN}" ]]; then return 0; fi
	if ${file}; then
		show_file "${SHOWN}" "${shown}"
	else
		show_lines "${SHOWN}" "${max}"
	fi
}
# offscreen WHAT CMD...: a check the screen does not show; its output goes to RAW and the log.
offscreen() {
	local what="$1" rc=0
	shift
	log "(off screen, ${what}: $*)"
	"$@" >"${RAW}" 2>&1 || rc=$?
	redact <"${RAW}" >>"${STEP_LOG}"
	log "(exit ${rc})"
	return "${rc}"
}
# journal SERVER SESSION: the session's journal (agentctl replay) in RAW and the log.
journal() { offscreen "the journal of $2" "${WORK}/bin/agentctl" replay --server "$1" --session "$2" || fail "agentctl replay $2 failed"; }
# last_seq KIND: the seq of the last record of KIND in RAW's journal.
last_seq() { python3 -c 'import re, sys; s = [int(m.group(1)) for m in re.finditer(r"^seq=(\d+) +" + sys.argv[1] + r"\b", open(sys.argv[2]).read(), re.M)]; print(s[-1] if s else "")' "$1" "${RAW}"; }
count() { python3 -c 'import re, sys; print(len(re.findall(r"^seq=\d+ +" + sys.argv[1] + r"\b", open(sys.argv[2]).read(), re.M)))' "$1" "${RAW}"; }
# lines_hold FILE CHECK...: each CHECK is "N:TEXT", line N of FILE must contain TEXT (the lines a
# split screen highlights hold what its caption says).
lines_hold() {
	local f="$1" c
	shift
	for c in "$@"; do
		[[ "$(sed -n "${c%%:*}p" "${f}")" == *"${c#*:}"* ]] || fail "line ${c%%:*} of ${f} does not hold ${c#*:}"
	done
}
# ends_at FILE N: FILE has exactly N lines, so the bat ranges on screen, which end at N, show it whole.
ends_at() {
	local n
	n="$(python3 -c 'import sys; print(len(open(sys.argv[1]).read().splitlines()))' "$1")"
	log "(off screen: ${1} has ${n} lines; the screens show 1-${2})"
	[[ "${n}" == "$2" ]] || fail "${1} has ${n} lines, not the ${2} its screens show"
}
# diagram COLOR NAME: a screen the recording replaces with a diagram (the recorder renders the Mermaid
# source and splice.py overlays it): every row below the caption is COLOR (magenta or green), which
# splice.py looks for.
diagram() {
	local rgb _
	case "$1" in
	magenta) rgb="255;0;255" ;;
	green) rgb="0;255;0" ;;
	*) fail "diagram: unknown color $1" ;;
	esac
	for _ in $(seq $((ROWS - HEADER_ROWS - 1))); do printf '\e[48;2;%sm%*s\e[0m\n' "${rgb}" "${COLS}" ''; done
	log "(on screen: the ${2} diagram, demo/${2}.mmd)"
	sleep "${DIAGRAM_HOLD}"
}
