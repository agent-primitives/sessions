# journal.awk: a summary of `agentctl replay -v` output, then the last turn's model call, answer and
# usage records with their hashes. Each record is a "seq=" line; an answer's later lines are not.
#
#   agentctl replay -v --server $API --session $COPY | awk -f demo/journal.awk
/^seq=/ {
	n++
	kind = $2
	sub(/^EVENT_/, "", kind)
	hash = prev = ""
	for (i = 3; i <= NF; i++) {
		if ($i ~ /^hash=/) hash = substr($i, 6)
		if ($i ~ /^prev=/) prev = substr($i, 6)
		if ($i ~ /^model=/) model = substr($i, 7)
		if ($i ~ /^input_tokens=/) tin += substr($i, 14)
		if ($i ~ /^output_tokens=/) tout += substr($i, 15)
	}
	head = hash
	if (kind == "END") turns++
	if (kind == "MODEL_CALL") calls++
	extra = ""
	if (kind == "MODEL_CALL") extra = "model=" model
	if (kind == "OUTPUT") {
		text = $0
		sub(/.*assistant: /, "", text)
		gsub(/\*\*/, "", text)
		extra = "\"" (length(text) > 30 ? substr(text, 1, 30) "…" : text) "\""
	}
	if (kind == "USAGE") {
		extra = ""
		for (i = 3; i <= NF; i++) if ($i ~ /^(input|output)_tokens=/) extra = extra $i " "
	}
	rec[n] = sprintf("%-7s %-11s hash=%.12s prev=%.12s  %s", $1, kind, hash, prev, extra)
	kinds[n] = kind
}
END {
	printf "%d turns, %d model calls (%s), %d tokens in / %d out, %d records, chain head %.16s\n", turns, calls, model, tin, tout, n, head
	for (i = 1; i <= n; i++) if (kinds[i] == "MODEL_CALL") last = i
	for (i = last; i <= n && i < last + 3; i++) print rec[i]
}
