package runner

import (
	"fmt"
	"strings"

	"github.com/Hamdan85/productos-agent/internal/protocol"
)

// buildPrompt frames the change definition for an unattended tool. The definition itself is never logged.
func buildPrompt(start protocol.RunStart, dir, branch string, asksWithTool bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are running unattended through the ProductOS Agent, in the git checkout at %s, on the branch `%s` (created from `%s`). Implement the change definition that follows.\n\n", dir, branch, start.Repository.DefaultBranch)
	b.WriteString("Rules:\n")
	b.WriteString("- Work only inside this checkout. Do not switch branches, do not push, do not open pull requests: the agent commits what you leave uncommitted, pushes the branch and opens the pull request when you finish.\n")
	if start.Options.RunTests {
		b.WriteString("- Run the project's relevant tests after your changes and fix what you broke; add the tests the test plan asks for.\n")
	}
	if start.Options.CommitTrailer != "" {
		fmt.Fprintf(&b, "- If you commit, end every commit message with the trailer `%s`.\n", start.Options.CommitTrailer)
	}
	if asksWithTool {
		b.WriteString("- If a decision needs the person, ask it with the AskUserQuestion tool and stop; the answer will arrive in a follow-up. Do not ask about anything you can decide yourself.\n")
	} else {
		b.WriteString("- If a decision needs the person, stop and end your reply with a single clear question; the answer will arrive in a follow-up. Do not ask about anything you can decide yourself.\n")
	}
	b.WriteString("- Finish with a short summary of what changed and how it was verified.\n\n")
	b.WriteString("----\n\n")
	b.WriteString(start.ChangeDefinition.Markdown)
	if !strings.HasSuffix(start.ChangeDefinition.Markdown, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// answerPrompt frames the person's answer for the resumed session.
func answerPrompt(question, answer string) string {
	q := strings.TrimSpace(question)
	if len(q) > 500 {
		q = q[:500] + "…"
	}
	return fmt.Sprintf("The person answered your question (%q):\n\n%s\n\nContinue the work with this answer; do not ask it again.\n", q, strings.TrimSpace(answer))
}

// looksLikeQuestion: a final message whose last non-empty line ends with a question mark.
func looksLikeQuestion(text string) bool {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		return strings.HasSuffix(l, "?")
	}
	return false
}
