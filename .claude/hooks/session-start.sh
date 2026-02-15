#!/bin/bash
set -euo pipefail

# Only run in remote (Claude Code on the web) environments
if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

GO_VERSION="1.25.7"
INSTALLED_VERSION=$(/usr/local/go/bin/go env GOVERSION 2>/dev/null || echo "none")

# Install the required Go version if not already present
if [ "$INSTALLED_VERSION" != "go${GO_VERSION}" ]; then
  echo "Installing Go ${GO_VERSION}..."
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -C /usr/local -xzf -
fi

# Download module dependencies
cd "$CLAUDE_PROJECT_DIR"
/usr/local/go/bin/go mod download
