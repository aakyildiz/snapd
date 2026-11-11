#!/bin/sh
# The fake store gives up a graceful stop after 2s with a long download in
# flight and exits 1; a stop the harness asked for is not a crash. Any exit
# not caused by a stop request keeps its status.
fakestore run --dir /srv/store --addr 0.0.0.0:11028 &
pid=$!
trap 'kill -TERM $pid 2>/dev/null; wait $pid; exit 0' TERM INT
wait $pid
