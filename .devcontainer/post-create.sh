#!/usr/bin/env bash
set -euo pipefail

# Fresh named volumes mount as root:root; chown so vscode can write.
sudo chown -R vscode:vscode /home/vscode/.claude

# Make bash functions available in both interactive and non-interactive shells.
cp /workspaces/google-drive-cleanup/.devcontainer/.bash_googledrivecleanup_functions ~/.bash_googledrivecleanup_functions
cat /workspaces/google-drive-cleanup/.devcontainer/.bashrc >> ~/.bashrc
cat /workspaces/google-drive-cleanup/.devcontainer/.bash_profile >> ~/.bash_profile
