# snapd Antithesis workload: status

## Waiting on you
- Nothing right now. Credentials work (orbitinghail via ~/.localrc).

## Local validation
- Last local run (7 min, eventually after 4 min): every assertion was reached; the end-of-test checks ran and the driver exited 0. Only the known HTTP 500 finding failed.

## Budget
- 200 compute hours. Runs 1-5 failed in setup (about 1.5 CH total): Antithesis' seccomp profile (fixed with seccomp=unconfined), tar/fchmodat2 (fixed: rootfs bind-mounted), Antithesis runtime not visible in the nested container (fixed: recursive bind of /opt/antithesis).
- Run 6 300a4922d4faac0952ca9b16d7683742-67-0 (12 CH): workload running on Antithesis. Triage in progress.

## Findings so far (both confirmed on Antithesis in run 6)
1. **An aborted change reports `Done` when nothing was done.** If you abort a sideload install before its first undoable task runs, the change ends `Done` with no error, and the snap is not installed. Cause: abort sets pending tasks to `Hold`, and tasks with no undo handler (`prerequisites`) go back to `Done` (`overlord/state/taskrunner.go:447`). `Change.Status()` ranks `Done` above `Hold` (`overlord/state/change.go:288`).
2. **Updating a disabled snap returns HTTP 500.** Sideloading a new revision of a disabled snap fails with `500 Internal Server Error` ("cannot update disabled snap"), for both single and multi-snap requests. This is a client error and should be a 4xx.
