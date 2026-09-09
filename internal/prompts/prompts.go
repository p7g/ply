package prompts

import _ "embed"

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
var Approval string
