package claude

import "regexp"

var exitCodeRE = regexp.MustCompile(`(?m)^Exit code:? (\d+)`)
