# This file is the project's own.
# Add recipes leveraging provided `do` ready-made recipes, or create your own.
# The import must be kept: it mounts every shared limen task under `just do ...`.
import '.limen/just/main.just'

# The FIRST recipe defined here becomes `just`'s default.
lint: do::lint::default do::lint::go::default
fix: do::fix::default do::fix::go::default
test: do::test::go::unit do::test::go::race do::test::go::cover
security: do::security::default do::security::go::default

# The command, built from this module at the commit checked out and run on
# the network the devices are on: `just upnp browse`, `just upnp describe URL`.

# Positional, so an argument with a space in it reaches the command as one.
[doc('Build upnp and run it: `just upnp browse`.')]
[positional-arguments]
upnp *args: build-upnp
    .build/upnp "$@"

build-upnp:
    mkdir -p .build
    go build -o .build/upnp ./cmd/upnp
