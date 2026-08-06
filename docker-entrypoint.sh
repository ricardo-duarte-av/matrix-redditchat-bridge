#!/bin/sh
# Bootstraps the bridge from a single mounted directory, one step per run.
#
#   1. no config.yaml            -> write one, then exit so it can be edited
#   2. no registration.yaml      -> generate the registration(s), then exit so they can be
#                                   installed on the homeserver
#   3. everything present        -> run the bridge
#
# Each step exits deliberately: the operator has to do something between them (fill in the
# config, install the registrations and restart Synapse), and starting anyway would just fail in
# a way that is harder to read.
set -eu

DATA_DIR="${BRIDGE_DATA_DIR:-/data}"
CONFIG="$DATA_DIR/config.yaml"
REGISTRATION="$DATA_DIR/registration.yaml"
DOUBLEPUPPET="$DATA_DIR/doublepuppet-registration.yaml"
BIN=/usr/local/bin/matrix-redditchat

# Anything after the entrypoint is passed straight through, so `docker compose run bridge --version`
# and similar still work.
if [ "$#" -gt 0 ]; then
    exec "$BIN" "$@"
fi

mkdir -p "$DATA_DIR"

# --- step 1: config ----------------------------------------------------------------------
if [ ! -f "$CONFIG" ]; then
    echo "No config.yaml in $DATA_DIR, writing one."
    "$BIN" -c "$CONFIG" -e

    # Point the database at the mounted directory, and listen on all interfaces so the
    # homeserver can reach the bridge from outside the container.
    sed -i \
        -e 's|^    type: postgres$|    type: sqlite3-fk-wal|' \
        -e "s|^    uri: postgres://.*\$|    uri: file:$DATA_DIR/bridge.db?_txlock=immediate|" \
        -e 's|^    hostname: .*$|    hostname: 0.0.0.0|' \
        -e 's|^    address: http://localhost:29340$|    address: http://CHANGE-ME:29340|' \
        "$CONFIG"

    cat <<EOF

Wrote $CONFIG. Edit it before starting again:

  homeserver.address   your homeserver's client-server API
  homeserver.domain    your server_name
  appservice.address   how the HOMESERVER reaches this container, e.g.
                       http://matrix-redditchat:29340 if both are on the same docker network
  bridge.permissions   who may use the bridge

Then start the container again to generate the registration files.
EOF
    exit 0
fi

# --- step 2: registrations ---------------------------------------------------------------
# The double puppet registration is only generated when the config asks for it, i.e. when
# double_puppet.secrets has no entry yet for this homeserver. If a secret is already configured
# (an externally managed appservice, or another server's) it is left alone.
want_doublepuppet() {
    domain=$(sed -n 's|^    domain: *||p' "$CONFIG" | head -1)
    [ -n "$domain" ] || return 1
    # Already configured for this domain? Then nothing to generate.
    grep -qE "^        ${domain}: *(\"|')?as_token:" "$CONFIG" && return 1
    return 0
}

if [ ! -f "$REGISTRATION" ] || { want_doublepuppet && [ ! -f "$DOUBLEPUPPET" ]; }; then
    if [ ! -f "$REGISTRATION" ]; then
        echo "Generating $REGISTRATION"
        "$BIN" -c "$CONFIG" -r "$REGISTRATION" -g
    fi
    if want_doublepuppet && [ ! -f "$DOUBLEPUPPET" ]; then
        echo "Generating $DOUBLEPUPPET"
        # This writes into the working directory, which is the data dir.
        (cd "$DATA_DIR" && "$BIN" -c "$CONFIG" --generate-doublepuppet-registration)
    fi

    cat <<EOF

Registration files are in $DATA_DIR. Install them on your homeserver:

  app_service_config_files:
    - /path/to/registration.yaml
$( [ -f "$DOUBLEPUPPET" ] && echo "    - /path/to/doublepuppet-registration.yaml" )

If a double_puppet secret was printed above, paste it into config.yaml.
Restart the homeserver, then start this container again to run the bridge.
EOF
    exit 0
fi

# --- step 3: run -------------------------------------------------------------------------
echo "Starting bridge with $CONFIG"
exec "$BIN" -c "$CONFIG" -r "$REGISTRATION"
