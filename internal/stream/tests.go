package stream

import "regexp"

// Test runners and the usual wrappers, anchored at a command boundary (start, separator, quote or space) so that
// `cat go test.log` is not a test but `bash -lc 'npm test'` is.
var testCommand = regexp.MustCompile(`(?i)(^|[;&|('"` + "`" + ` ]\s*)(` +
	`bundle\s+exec\s+rspec|rspec|bin/rspec|(?:bin/)?rails\s+test|bin/ci|` +
	`npm\s+(?:run\s+)?test|pnpm\s+(?:run\s+)?test|yarn\s+(?:run\s+)?test|bun\s+test|` +
	`npx\s+(?:vitest|jest|mocha|playwright\s+test)|vitest|jest|mocha|` +
	`pytest|py\.test|python3?\s+-m\s+(?:pytest|unittest)|` +
	`go\s+test|cargo\s+test|mix\s+test|phpunit|vendor/bin/phpunit|` +
	`mvn\s+(?:-\S+\s+)*test|gradle\s+test|\./gradlew\s+test|dotnet\s+test|` +
	`make\s+(?:test|check)|swift\s+test|flutter\s+test|dart\s+test|ctest` +
	`)(\s|$)`)
