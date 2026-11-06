package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/agent-primitives/sessions/api/genpb"
)

func sessionFlags(fs *flag.FlagSet) *config {
	cfg := commonFlags(fs)
	fs.BoolVar(&cfg.json, "json", false, "print protobuf JSON (replay: one LogRecord per line)")
	return cfg
}

func stateName(value, prefix string) string {
	return strings.TrimPrefix(strings.TrimPrefix(value, prefix), "STATE_")
}

func writeOutput(text string) error {
	if _, err := fmt.Fprint(os.Stdout, text); err != nil {
		return fmt.Errorf("write response to stdout (successful operations remain committed): %w", err)
	}
	return nil
}

func writeJSON(message proto.Message) error {
	data, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(message)
	if err != nil {
		return fmt.Errorf("encode protobuf JSON response (successful operations remain committed): %w", err)
	}
	return writeOutput(escapeFormatRunes(string(data)) + "\n")
}

func sessionDetails(s *v1.Session) string {
	var out strings.Builder
	fields := [][2]string{
		{"Session", escapeText(s.GetMetadata().GetUid())},
		{"Name", escapeText(s.GetMetadata().GetName())},
		{"Project", escapeText(s.GetMetadata().GetProject())},
		{"Harness", escapeText(s.GetHarness())},
		{"Model", escapeText(s.GetModel())},
		{"Execution", stateName(s.GetExecState().String(), "EXEC_")},
		{"Compute", stateName(s.GetComputeState().String(), "COMPUTE_")},
		{"Last seq", strconv.FormatInt(s.GetLastSeq(), 10)},
		{"Deletion", stateName(s.GetDeletionState().String(), "DELETION_")},
		{"Deletion requested", deletionTime(s.GetDeletionRequestedTime())},
	}
	if s.GetParentUid() != "" {
		fields = append(fields, [2]string{"Parent", escapeText(s.GetParentUid())},
			[2]string{"Fork seq", strconv.FormatInt(s.GetForkSeq(), 10)})
	}
	for _, field := range fields {
		fmt.Fprintf(&out, "%-20s%s\n", field[0], field[1])
	}
	return out.String()
}

func sessionTable(sessions []*v1.Session) string {
	rows := [][]string{{"SESSION", "NAME", "EXECUTION", "COMPUTE", "HARNESS", "LAST SEQ"}}
	for _, s := range sessions {
		rows = append(rows, []string{
			displayToken(s.GetMetadata().GetUid()), displayToken(s.GetMetadata().GetName()),
			stateName(s.GetExecState().String(), "EXEC_"),
			stateName(s.GetComputeState().String(), "COMPUTE_"),
			displayToken(s.GetHarness()), strconv.FormatInt(s.GetLastSeq(), 10),
		})
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, value := range row {
			widths[i] = max(widths[i], utf8.RuneCountInString(value))
		}
	}
	var out strings.Builder
	for _, row := range rows {
		for i, value := range row {
			width := utf8.RuneCountInString(value)
			out.WriteString(value)
			if i != len(row)-1 {
				out.WriteString(strings.Repeat(" ", widths[i]-width+2))
			}
		}
		out.WriteByte('\n')
	}
	return out.String()
}

func deletionFields(s *v1.Session) string {
	return fmt.Sprintf("deletion=%s deletion_requested=%s",
		stateName(s.GetDeletionState().String(), "DELETION_"),
		deletionTime(s.GetDeletionRequestedTime()))
}
