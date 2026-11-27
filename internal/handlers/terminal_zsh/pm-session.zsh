# Patchman terminal: tell the sidebar what runs here. Read by
# patchman-ui/src/components/terminal/terminalSessions.ts.
#   OSC 6973 = command line that just started; OSC 7 = back at the prompt + cwd.
autoload -Uz add-zsh-hook
_pm_session_preexec() { printf '\e]6973;%s\a' "${1//[[:cntrl:]]/ }"; }
_pm_session_precmd() { printf '\e]7;file://%s%s\a' "$HOST" "${${${PWD//[[:cntrl:]]/?}//\%/%25}// /%20}"; }
add-zsh-hook preexec _pm_session_preexec
add-zsh-hook precmd _pm_session_precmd
# Closing a session SIGKILLs the shell; write each command as it runs so it
# isn't lost. Users who already share/append history are left alone.
[[ -o sharehistory || -o incappendhistory || -o incappendhistorytime ]] || setopt incappendhistory
