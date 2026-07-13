#!/usr/bin/env bash
set -euo pipefail

# Fresh named volumes mount as root:root; chown so vscode can write.
sudo chown -R vscode:vscode /home/vscode/.claude

# Make bash functions available in both interactive and non-interactive shells.
cp /workspaces/google-drive-cleanup/.devcontainer/.bash_googledrivecleanup_functions ~/.bash_googledrivecleanup_functions
cat /workspaces/google-drive-cleanup/.devcontainer/.bashrc >> ~/.bashrc
cat /workspaces/google-drive-cleanup/.devcontainer/.bash_profile >> ~/.bash_profile

# Playwright MCP (.mcp.json) needs its browser binary and system libs, neither
# of which npx pulls in automatically.
npx --yes playwright install chromium-headless-shell
sudo env "PATH=$PATH" npx --yes playwright install-deps chromium
