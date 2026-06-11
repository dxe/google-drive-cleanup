
# Load shared google-drive-cleanup shell functions.
if [ -f ~/.bash_googledrivecleanup_functions ]; then
  . ~/.bash_googledrivecleanup_functions
fi

# Add git support for bash-completion. Assumes bash-completion apt package is
# installed. devcontainer.json should install it automatically.
source /usr/share/bash-completion/completions/git
