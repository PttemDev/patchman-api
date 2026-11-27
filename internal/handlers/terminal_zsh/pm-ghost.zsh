# Patchman terminal: gray inline suggestion from history, accepted with →, End/Ctrl-E (⌘→);
# one word with ⌥→/ESC f.
(( $+functions[_zsh_autosuggest_start] )) && return   # user runs zsh-autosuggestions
autoload -Uz is-at-least add-zle-hook-widget
is-at-least 5.9 || return                             # region_highlight memo= needs 5.9

_pm_ghost_clear() {
  POSTDISPLAY=
  region_highlight=("${(@)region_highlight:#*memo=pm_ghost}")
}

_pm_ghost_update() {
  emulate -L zsh
  setopt extendedglob
  _pm_ghost_clear
  [[ -n $BUFFER ]] && (( CURSOR == $#BUFFER )) || return
  # $history values come newest first, so (r) yields the newest single-line match.
  # The newline sits in a variable: $'\n' is not expanded inside the subscript.
  local nl=$'\n'
  local match=${history[(r)${(b)BUFFER}[^$nl]##]}
  [[ -n $match ]] || return
  POSTDISPLAY=${match:$#BUFFER}
  region_highlight+=("$#BUFFER $(( $#BUFFER + $#POSTDISPLAY )) fg=8 memo=pm_ghost")
}

_pm_ghost_shown() { [[ -n $POSTDISPLAY ]] && (( CURSOR == $#BUFFER )) }

_pm_ghost_take_all() { BUFFER+=$POSTDISPLAY; CURSOR=$#BUFFER; _pm_ghost_clear }

_pm_ghost_accept() {
  if _pm_ghost_shown; then
    _pm_ghost_take_all
  else
    zle .forward-char
  fi
}

_pm_ghost_accept_eol() {
  if _pm_ghost_shown; then
    _pm_ghost_take_all
  else
    zle .end-of-line
  fi
}

_pm_ghost_accept_word() {
  if _pm_ghost_shown; then
    BUFFER+=$POSTDISPLAY; _pm_ghost_clear; zle .emacs-forward-word; BUFFER=${BUFFER[1,CURSOR]}
  else
    zle .forward-word
  fi
}

zle -N forward-char _pm_ghost_accept
zle -N end-of-line _pm_ghost_accept_eol
zle -N forward-word _pm_ghost_accept_word
add-zle-hook-widget line-pre-redraw _pm_ghost_update
add-zle-hook-widget line-finish _pm_ghost_clear
