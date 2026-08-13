# fish completion for cccp
# Install to ~/.config/fish/completions/cccp.fish

complete -c cccp -f

# Subcommands (only when no subcommand picked yet).
complete -c cccp -n __fish_use_subcommand -a serve   -d "Run the storage server"
complete -c cccp -n __fish_use_subcommand -a push    -d "Ship local state to the server"
complete -c cccp -n __fish_use_subcommand -a pull    -d "Pull remote state to local disk"
complete -c cccp -n __fish_use_subcommand -a watch   -d "Watch ~/.claude and sync on change"
complete -c cccp -n __fish_use_subcommand -a explore -d "Browse local sessions as rich timelines"
complete -c cccp -n __fish_use_subcommand -a host-id -d "Print this host's persistent UUID"
complete -c cccp -n __fish_use_subcommand -a help    -d "Show usage"

# Flags available on every subcommand except `explore` (which has its own
# minimal flag set -see below).
set -l common "not __fish_seen_subcommand_from explore"
complete -c cccp -n "$common" -l dir        -r -F -d "Config root directory"
complete -c cccp -n "$common" -l url        -r    -d "Server URL (e.g. https://host:12110)"
complete -c cccp -n "$common" -l server-url -r    -d "Alias for --url"
complete -c cccp -n "$common" -l token      -r    -d "Bearer token"
complete -c cccp -n "$common" -l addr       -r    -d "Listen address (serve)"
complete -c cccp -n "$common" -l address    -r    -d "Alias for --addr"
complete -c cccp -n "$common" -l port       -r    -d "Listen port (overrides --addr port)"
complete -c cccp -n "$common" -l db         -r -F -d "SQLite database path"
complete -c cccp -n "$common" -l database   -r -F -d "Alias for --db"
complete -c cccp -n "$common" -l db-path    -r -F -d "Alias for --db"
complete -c cccp -n "$common" -l config     -r -F -d "Path to JSON config file"
complete -c cccp -s h -l help                    -d "Show help"

# pull-only flag.
complete -c cccp -n "__fish_seen_subcommand_from pull" -l full -d "Bootstrap (rewrite everything)"

# push-only flag.
complete -c cccp -n "__fish_seen_subcommand_from push" -l cheka-allow -r -d "Tolerate up to N secret findings (-1 disables)"
complete -c cccp -n "__fish_seen_subcommand_from push" -l cheka-redact -d "Redact secrets in log files instead of blocking"

# explore-only flags.
complete -c cccp -n "__fish_seen_subcommand_from explore" -s p -l port -r -d "HTTP port (default 9123)"

# watch sub-subcommands.
complete -c cccp -n "__fish_seen_subcommand_from watch; and not __fish_seen_subcommand_from hook-start hook-end status stop" -a hook-start -d "SessionStart hook (reads JSON on stdin)"
complete -c cccp -n "__fish_seen_subcommand_from watch; and not __fish_seen_subcommand_from hook-start hook-end status stop" -a hook-end   -d "SessionEnd hook (reads JSON on stdin)"
complete -c cccp -n "__fish_seen_subcommand_from watch; and not __fish_seen_subcommand_from hook-start hook-end status stop" -a status     -d "Print daemon status"
complete -c cccp -n "__fish_seen_subcommand_from watch; and not __fish_seen_subcommand_from hook-start hook-end status stop" -a stop       -d "SIGTERM the daemon"

# watch-only flag.
complete -c cccp -n "__fish_seen_subcommand_from watch" -l debounce-ms -r -d "Debounce window in ms (default 500)"
complete -c cccp -n "__fish_seen_subcommand_from watch" -l cheka-allow -r -d "Tolerate up to N secret findings (-1 disables)"
complete -c cccp -n "__fish_seen_subcommand_from watch" -l cheka-redact -d "Redact secrets in log files instead of blocking"
