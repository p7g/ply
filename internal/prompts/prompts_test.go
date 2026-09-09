package prompts

import (
	"fmt"
	"strings"
	"testing"
)

func TestApprovalTemplateQuotesContextAsData(t *testing.T) {
	value := "quotes \"; newline\n{{.PlanMode}} <tag> $HOME"
	context := ApprovalContext{
		Command: value, UserRequest: value, Justification: value,
		WorkingDirectory: value, TranscriptPath: value, TimeoutSeconds: "120", SubagentDepth: "0",
	}
	output, err := RenderApproval(context)
	if err != nil {
		t.Fatal(err)
	}
	for label, value := range map[string]string{
		"Requested shell command":                            context.Command,
		"User request":                                       context.UserRequest,
		"Command justification":                              context.Justification,
		"Working directory":                                  context.WorkingDirectory,
		"Conversation transcript path":                       context.TranscriptPath,
		"Timeout in seconds":                                 context.TimeoutSeconds,
		"Subagent nesting depth (zero means the main agent)": context.SubagentDepth,
	} {
		want := fmt.Sprintf("%s: %q\n", label, value)
		if !strings.Contains(output, want) {
			t.Fatalf("missing quoted context %q", want)
		}
	}
	if strings.Contains(output, "&lt;") || strings.Contains(output, "newline\n{{") {
		t.Fatal("context was HTML escaped or inserted without quoting")
	}
}

func TestApprovalTemplateModes(t *testing.T) {
	for _, plan := range []bool{false, true} {
		for _, background := range []bool{false, true} {
			for _, truncated := range []bool{false, true} {
				output, err := RenderApproval(ApprovalContext{PlanMode: plan, Background: background, CommandTruncated: truncated})
				if err != nil {
					t.Fatal(err)
				}
				pairs := []struct {
					enabled bool
					on, off string
				}{
					{plan, "Planning status: plan mode; inspection only, no implementation mutations.", "Planning status: execution mode."},
					{background, "Execution: starts a background task.", "Execution: runs in the foreground."},
					{truncated, "The command was truncated; its complete effects cannot be assessed.", "The full command is included."},
				}
				for _, pair := range pairs {
					if strings.Contains(output, pair.on) != pair.enabled || strings.Contains(output, pair.off) == pair.enabled {
						t.Fatalf("wrong context branch: plan=%v background=%v truncated=%v\n%s", plan, background, truncated, output)
					}
				}
			}
		}
	}
}
