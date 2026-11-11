# snapd Antithesis workload: status

## Waiting on you
- Nothing right now. Credentials work (orbitinghail via ~/.localrc).

## Local validation
- Last local run (7 min, eventually after 4 min): every assertion was reached; the end-of-test checks ran and the driver exited 0. Only the known HTTP 500 finding failed.

## Budget
- 200 compute hours. Runs 1-5 failed in setup (about 1.5 CH total): Antithesis' seccomp profile (fixed with seccomp=unconfined), tar/fchmodat2 (fixed: rootfs bind-mounted), Antithesis runtime not visible in the nested container (fixed: recursive bind of /opt/antithesis).
- Run 6 300a4922d4faac0952ca9b16d7683742-67-0 (12 CH, cancelled at 13 min): first run with the workload running. Triaged; see findings 3 and workload fixes below.
- Run 7 109329e247610f368f67a2c132fd978e-67-0 (24 CH): running.
- Estimated spend so far: ~25 CH (setup-timeout runs ~4 CH each; run 6 counted at its 12 CH cap).

## Findings so far (both confirmed on Antithesis in run 6)
1. **An aborted change reports `Done` when nothing was done.** If you abort a sideload install before its first undoable task runs, the change ends `Done` with no error, and the snap is not installed. Cause: abort sets pending tasks to `Hold`, and tasks with no undo handler (`prerequisites`) go back to `Done` (`overlord/state/taskrunner.go:447`). `Change.Status()` ranks `Done` above `Hold` (`overlord/state/change.go:288`).
2. **Updating a disabled snap returns HTTP 500.** Sideloading a new revision of a disabled snap fails with `500 Internal Server Error` ("cannot update disabled snap"), for both single and multi-snap requests. This is a client error and should be a 4xx.
3. **A failed snap mount leaks its snap file and mount unit file forever** (found on Antithesis, run 6). If `systemctl daemon-reload` fails while the mount task runs (here systemd was going down for a reboot), snapd tries to clean up. But `RemoveSnapFiles` (`overlord/snapstate/backend/setup.go:280`) returns as soon as `removeMountUnit` fails, so `/var/lib/snapd/snaps/<name>_<rev>.snap` is never deleted. `RemoveMountUnitFile` (`systemd/systemd.go`) likewise returns before `os.Remove(unit)`. The task ends in Error, and nothing retries the cleanup after the reboot. Example: the moment with input_hash 2749198260823943087 at vtime 28.18 leaked `tst-c_x1.snap` and `snap-tst\x2dc-x1.mount`.
4. *(Possible, not yet confirmed by its own property.)* **An aborted remove can delete both the snap's data and its automatic snapshot.** `save-snapshot` is undone with forget after `clear-snap` has already removed the data.

5. **An undone remove can leave a snap disabled** (found on Antithesis, run 7, change 207 at input_hash 7336599675049985534, vtime 64.24). Before re-enabling a snap, `undoUnlinkSnap` (`overlord/snapstate/handlers.go:3450`) checks that `/var/snap/<snap>/<rev>` and `/var/snap/<snap>/common` exist, as a stand-in for "this change's `clear-snap` ran". Here an earlier aborted remove had already deleted `common` (its `clear-snap` has no undo). A later enable succeeded without recreating it. Then remove 207 was aborted before its own `clear-snap` ran (the task is on Hold), and its undo refused to re-enable the snap. The change reports Undone, but the snap ends up disabled, and a user has no way to tell. It is related to finding 4: data loss from an aborted remove is never repaired, and snapd's later decisions inherit it.

6. **A crash during `discard-snap`, followed by an abort, leaves snapd tracking a revision whose files are deleted** (run 7, input_hash -8539997110608008820, vtime 175.68). `doDiscardSnap` (`overlord/snapstate/handlers.go:3598`) deletes the revision's blob, mount unit and mount (`RemoveSnapFiles`, line ~3645) before it saves the shorter sequence (`Set`, line ~3717). A container kill inside that window of about 100 ms left `tst-a` x1 in snapd's sequence with no files. After the restart, the driver's abort turned the interrupted task into Hold (`tryUndo`: no undo handler and stopped in flight, so Hold). The task never ran again. Change 301 reports Done (finding 1), yet snapd still lists x1, so a revert to it would fail.
7. **`discard-snap` is not safe to run twice: after a crash, snapd can forget an installed, active snap** (run 7, input_hash -7579822346999364605, vtime 236.88). Change 353 removed only revision x2 of `tst-a`, leaving x1 current. Its discard-snap finished, including `Set`, but snapd was SIGKILLed before the task was saved as Done. On restart the task ran again for x2. The sequence was then `[x1]`, and `if len(Sequence.Revisions) == 1 { Revisions = nil }` (line ~3620) cleared it without checking that x2 was no longer in it. The handler then deleted the whole `tst-a` entry from state. x1 stays mounted and active, with its blob, mount unit and data, but snapd says `tst-a` isn't installed. **This is the most severe finding so far.**
   - Suggested fix: in doDiscardSnap, if the revision is no longer in the sequence, only retry `RemoveSnapFiles` and return. Save the new sequence before deleting files. And don't put an interrupted no-undo task on Hold; let it finish.

8. **A crash during a sideload's `mount-snap` leaks the mounted revision permanently** (run 7, input_hash 8136618061046659039, vtime 157.29; 11 failures, all from one history). `doMountSnap` (`overlord/snapstate/handlers.go`) does the following in order:
   1. It sets up the blob, mount unit and mount (line ~899).
   2. It deletes the uploaded `.local-install-*` file (line ~948).
   3. Only after that does it mark the task Done (line ~965).

   A container kill between steps 2 and 3 makes the task re-run after the restart. `checkSnap` then fails on the missing upload (line ~852), before any cleanup is set up, and the task ends in Error, which the task runner never undoes. The blob, unit file and squashfuse mount stay behind for a snap that snapd's state doesn't have. Side observation: about 20 `.local-install-*` upload files from failed sideloads are left behind in `/var/lib/snapd/snaps`.

## Workload fixes from run 6 triage
- Host container: after a kill, incusd auto-started the instance before the rootfs bind mount was back, and the instance got stuck. Fixed: mount before incusd starts, `boot.autostart=false`, check existence by config, and retry the start.
- Atomicity model: `clear-snap` and `discard-snap` have no undo by design, so an abort that lands after them keeps their effect. The check now expects exactly those effects; there is a new exploration assertion for this case.
- Agent: also lists mount unit files that systemd hasn't loaded. The filesystem property is split into "leaked" and "missing or wrong".
