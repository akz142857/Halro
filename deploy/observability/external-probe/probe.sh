#!/bin/sh
# Compatibility entrypoint for existing automation. The production
# implementation is the independently built heimdall-deadman Go binary.
set -eu
exec /usr/local/bin/heimdall-deadman "$@"
