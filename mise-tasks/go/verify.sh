#!/bin/sh
#MISE description="Every offline Go check and the repository checks; go:vuln runs in CI on its own"
#MISE depends=["go:lint", "go:tidy", "go:fix", "go:test", "check:repo"]
# The one gate. Its dependencies run in parallel.
