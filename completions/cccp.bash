
_cccp() {
    local cur prev words cword split
    if declare -F _init_completion >/dev/null 2>&1; then
        _init_completion -s || return
    else
        COMPREPLY=()
        cur=${COMP_WORDS[COMP_CWORD]}
        prev=${COMP_WORDS[COMP_CWORD-1]}
        cword=$COMP_CWORD
    fi

    local commands="serve push pull watch explore index host-id help"
    local global_flags="--dir --url --server-url --token --addr --address --port --db --database --db-path --config -h --help"

    local cmd=""
    local i
    for (( i=1; i < cword; i++ )); do
        case "${COMP_WORDS[i]}" in
            serve|push|pull|watch|explore|index|host-id|help) cmd=${COMP_WORDS[i]}; break ;;
        esac
    done

    if [[ -z $cmd ]]; then
        COMPREPLY=( $(compgen -W "$commands" -- "$cur") )
        return 0
    fi

    case "$prev" in
        --dir)
            COMPREPLY=( $(compgen -d -- "$cur") )
            return 0
            ;;
        --db|--database|--db-path|--config)
            COMPREPLY=( $(compgen -f -- "$cur") )
            return 0
            ;;
        --url|--server-url|--token|--addr|--address|--port|--cheka-allow|--debounce-ms|-p)
            return 0
            ;;
    esac

    if [[ $cmd == watch ]]; then
        local watch_sub=""
        for (( i++; i < cword; i++ )); do
            case "${COMP_WORDS[i]}" in
                hook-start|hook-end|status|stop) watch_sub=${COMP_WORDS[i]}; break ;;
            esac
        done
        if [[ -z $watch_sub && $cur != -* ]]; then
            COMPREPLY=( $(compgen -W "hook-start hook-end status stop" -- "$cur") )
            return 0
        fi
    fi

    local flags=""
    case $cmd in
        pull)        flags="$global_flags --full" ;;
        push)        flags="$global_flags --cheka-allow --cheka-redact" ;;
        watch)       flags="$global_flags --debounce-ms --cheka-allow --cheka-redact" ;;
        explore)     flags="-p --port -h --help" ;;
        serve|host-id) flags="$global_flags" ;;
        index) flags="rebuild update status --fresh $global_flags" ;;
        help)        flags="" ;;
    esac

    if [[ $cur == -* ]]; then
        COMPREPLY=( $(compgen -W "$flags" -- "$cur") )
    fi
}
complete -F _cccp cccp
