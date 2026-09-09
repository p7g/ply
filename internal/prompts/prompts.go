package prompts

import (
	_ "embed"
	"strings"
	"text/template"
)

//go:embed system.txt
var System string

//go:embed plan.txt
var Plan string

//go:embed subagent.txt
var Subagent string

//go:embed no_tools.txt
var NoTools string

//go:embed compact.txt
var Compact string

//go:embed approval.txt
var approvalSource string

var approvalTemplate = template.Must(template.New("approval").Option("missingkey=error").Parse(approvalSource))

// ApprovalContext holds subprocess inputs as data, separate from prompt wording.
// Text fields retain their original values and are quoted by the template.
type ApprovalContext struct {
	Command, UserRequest, Justification, WorkingDirectory string
	TranscriptPath, TimeoutSeconds, SubagentDepth         string
	PlanMode, Background, CommandTruncated                bool
}

func RenderApproval(context ApprovalContext) (string, error) {
	var output strings.Builder
	if err := approvalTemplate.Execute(&output, context); err != nil {
		return "", err
	}
	return output.String(), nil
}
