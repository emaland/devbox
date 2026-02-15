#!/bin/bash
set -euo pipefail

# Only run in remote (web) environments
if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

GO_VERSION="1.25.7"
GO_INSTALL_DIR="/usr/local/go"

# Install the required Go version if not already present
current_version=$(${GO_INSTALL_DIR}/bin/go version 2>/dev/null | grep -oP 'go\K[0-9]+\.[0-9]+\.[0-9]+' || echo "none")
if [ "$current_version" != "$GO_VERSION" ]; then
  echo "Installing Go ${GO_VERSION} (current: ${current_version})..."
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tar.gz
  rm -rf "$GO_INSTALL_DIR"
  tar -C /usr/local -xzf /tmp/go.tar.gz
  rm /tmp/go.tar.gz
  echo "Go ${GO_VERSION} installed."
else
  echo "Go ${GO_VERSION} already installed."
fi

# Ensure Go is on PATH for the session
echo 'export PATH="/usr/local/go/bin:${HOME}/go/bin:${PATH}"' >> "$CLAUDE_ENV_FILE"

# Download module dependencies
cd "$CLAUDE_PROJECT_DIR"
/usr/local/go/bin/go mod download

echo "Go environment ready."
