
# Load shared drive-cleanup shell functions.
if [ -f ~/.bash_googledrivecleanup_functions ]; then
  . ~/.bash_googledrivecleanup_functions
fi

# Add git support for bash-completion. Assumes bash-completion apt package is
# installed. devcontainer.json should install it automatically.
source /usr/share/bash-completion/completions/git

### Useful aliases for development ###

# Load drive-completion bash completion for the drive-completion shell helper in interactive shells.
if [[ $- == *i* ]]; then
  # Store the generated completion script in a persistent cache so new shells
  # can source a file directly instead of running `go run` every time.
  _drive_completion_cache="${XDG_CACHE_HOME:-$HOME/.cache}/drive-completion.bash"
  _drive_completion_needs_refresh=0

  if [[ -d /workspaces/google-drive-cleanup ]]; then
    # Refresh if the cache is missing/empty, or if any Go source/module file in
    # the CLI project is newer than the cached completion script.
    if [[ ! -s "$_drive_completion_cache" ]]; then
      _drive_completion_needs_refresh=1
    elif find /workspaces/google-drive-cleanup -type f \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$_drive_completion_cache" -print -quit 2>/dev/null | grep -q .; then
      _drive_completion_needs_refresh=1
    fi

    # Only try to regenerate when Go is installed. Write to a temporary file
    # first so a failed generation does not leave a partial cache behind.
    if [[ $_drive_completion_needs_refresh -eq 1 ]] && command -v go >/dev/null 2>&1; then
      mkdir -p "$(dirname "$_drive_completion_cache")"
      if go run /workspaces/google-drive-cleanup completion bash >"${_drive_completion_cache}.tmp" 2>/dev/null; then
        mv "${_drive_completion_cache}.tmp" "$_drive_completion_cache"
      fi
      rm -f "${_drive_completion_cache}.tmp"
    fi
  fi

  # If a cached completion file exists, load it even when regeneration was
  # skipped (for example because Go is not currently installed).
  [[ -s "$_drive_completion_cache" ]] && source "$_drive_completion_cache"
  unset _drive_completion_cache _drive_completion_needs_refresh
fi
