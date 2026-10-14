package runner

import (
	"fmt"
	"strings"

	"github.com/productos-technology/productos-agent/internal/protocol"
)

// buildPrompt frames the change definition for an unattended tool. The definition itself is never logged. prior lists
// the commits on origin/<default> that already carry the run's trailer (an earlier run, or someone by hand).
func buildPrompt(start protocol.RunStart, dir, branch string, asksWithTool bool, prior []string) string {
	from := start.Repository.DefaultBranch
	var b strings.Builder
	fmt.Fprintf(&b, "You are running unattended through the ProductOS Agent, in the git checkout at %s, on the branch `%s` (created from `%s`). Implement the change definition that follows.\n\n", dir, branch, from)
	b.WriteString("Rules:\n")
	b.WriteString("- First check whether the change definition is already satisfied: read the code it concerns. If it is, change nothing and answer the report with claim `already_done` and its evidence (file:line, a commit, a pull request).\n")
	if len(prior) > 0 && start.Options.CommitTrailer != "" {
		fmt.Fprintf(&b, "- These commits on `origin/%s` already carry `%s`; look at them before you start:\n", from, start.Options.CommitTrailer)
		for _, c := range prior {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	b.WriteString("- Work only inside this checkout and write no file outside it. Do not switch branches, do not push, do not open pull requests: the agent commits what you leave uncommitted, pushes the branch and opens the pull request when you finish.\n")
	b.WriteString("- Never make cosmetic changes (formatting, comments, renames) just to have something to commit.\n")
	b.WriteString("- Do what the change definition asks and nothing more; what it asks that you did not do goes to `left_out` in the report (leave it empty when you did everything; the agent's own commit, push and pull request are not left out).\n")
	if start.Options.RunTests {
		b.WriteString("- Run the project's relevant tests after your changes and fix what you broke; add the tests the test plan asks for.\n")
	}
	if start.Options.CommitTrailer != "" {
		fmt.Fprintf(&b, "- If you commit, end every commit message with the trailer `%s`.\n", start.Options.CommitTrailer)
	}
	if asksWithTool {
		b.WriteString("- If a decision needs the person, ask it with the AskUserQuestion tool and stop; the answer will arrive in a follow-up. Do not ask about anything you can decide yourself.\n")
	} else {
		b.WriteString("- If a decision needs the person, stop and answer the report with claim `needs_input`, the question in `question` and the choices in `options`; the answer will arrive in a follow-up. Do not ask about anything you can decide yourself.\n")
	}
	b.WriteString("- If you cannot do it, say why and answer the report with claim `blocked`.\n")
	b.WriteString("- Finish with a short summary of what changed and how it was verified, then answer the final report (the JSON schema you were given) as your structured output: `changed` when you changed code, with the changes, the checks you ran in `verification` and, in `customer_change`, one sentence a customer would read in the changelog (null when nothing visible changed).\n")
	b.WriteString("- Write the summary and the report in the language of the change definition.\n\n")
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
		q = clip(q, 500)
	}
	return fmt.Sprintf("The person answered your question (%q):\n\n%s\n\nContinue the work with this answer; do not ask it again. When you finish, answer the final report again.\n", q, strings.TrimSpace(answer))
}

// looksLikeQuestion: a final message whose last non-empty line ends with a question mark (a tool that neither used a
// question tool nor answered the report).
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
