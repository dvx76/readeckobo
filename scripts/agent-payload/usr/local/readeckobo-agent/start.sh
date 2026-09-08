#!/bin/sh
# Readeck sync agent runner (on-device).
#
# Launched by the udev hook when Wi-Fi comes up (see udev_program.sh) or by a
# NickelMenu entry. Loops: wait for a usable network, run ONE sync pass, then
# sleep — so a dropped connection or a briefly unreachable server is retried
# on the next pass instead of killing the loop.
#
# Optional environment overrides:
#   RKB_CONFIG      config file path (default /mnt/onboard/.adds/readeckobo/config)
#   RKB_INTERVAL    seconds between passes (default 1800)
#   RKB_PING_HOST   host probed for connectivity (default 1.1.1.1)
#   RKB_WAIT_MAX    seconds to wait for connectivity before syncing anyway (default 300)

AGENT=/usr/local/readeckobo-agent/readeckobo-agent
CONFIG=${RKB_CONFIG:-/mnt/onboard/.adds/readeckobo/config}
INTERVAL=${RKB_INTERVAL:-1800}
PING_HOST=${RKB_PING_HOST:-1.1.1.1}
WAIT_MAX=${RKB_WAIT_MAX:-300}

if [ ! -x "$AGENT" ]; then
    echo "readeckobo-agent: binary missing at $AGENT" >&2
    exit 1
fi
if [ ! -f "$CONFIG" ]; then
    echo "readeckobo-agent: no config at $CONFIG; copy config.sample and set SERVER_URL/TOKEN" >&2
fi

wait_network() {
    waited=0
    while [ "$waited" -lt "$WAIT_MAX" ]; do
        if ping -q -c 1 -W 2 "$PING_HOST" >/dev/null 2>&1; then
            return 0
        fi
        sleep 3
        waited=$((waited + 3))
    done
    # Timeout: run anyway. The agent fails fast on an unreachable server and
    # the loop retries after the next sleep.
    return 0
}

while : ; do
    wait_network
    "$AGENT" --once --config "$CONFIG"
    sleep "$INTERVAL"
done
