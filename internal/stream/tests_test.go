package stream

import "testing"

func TestIsTestCommand(t *testing.T) {
	yes := []string{
		"npm test", "npm test -- drafts", "npm run test", "pnpm test", "yarn test", "bun test",
		"bundle exec rspec spec/requests", "rspec", "bin/rails test", "rails test test/models", "bin/ci",
		"go test ./...", "cargo test", "pytest -q", "python -m pytest", "python3 -m unittest",
		"mix test", "./gradlew test", "mvn -q test", "dotnet test", "make test", "cd app && npm test",
		"/bin/bash -lc 'npm test -- drafts'", "npx vitest run", "vitest", "jest --ci", "flutter test",
	}
	no := []string{
		"ls -la", "cat go test.log", "git status", "npm install", "npm run build", "go build ./...",
		"echo testing", "rspecial --help", "cargo build", "make", "contest", "latest", "pytestify",
	}
	for _, c := range yes {
		if !IsTestCommand(c) {
			t.Errorf("%q should be a test command", c)
		}
	}
	for _, c := range no {
		if IsTestCommand(c) {
			t.Errorf("%q should not be a test command", c)
		}
	}
}
