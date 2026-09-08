#!/bin/sh
# udev RUN helper for the Readeck sync agent.
#
# udev runs this when a network interface appears; udev may kill processes
# that are still attached to the event, so we detach into the background
# (setsid, the KoboCloud pattern) with a plain-background fallback for
# firmwares without setsid.
#
# See docs/AGENT.md for the full picture.

if command -v setsid >/dev/null 2>&1; then
    setsid /usr/local/readeckobo-agent/start.sh >/dev/null 2>&1 < /dev/null &
else
    /usr/local/readeckobo-agent/start.sh >/dev/null 2>&1 < /dev/null &
fi
exit 0
