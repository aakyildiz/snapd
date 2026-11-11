// Command agent runs in the Incus host container. It gives the driver
// out-of-band control over the snapd system container: crash injection and
// a view of the filesystem that snapd manages. It never observes snapd's API.
package main

import (
	"context"
	"log"
	"net/http"
	"os/exec"
	"time"
)

const container = "snapd"

// fsScript prints, one per line:
//
//	file <name>_<rev>.snap            for each blob in /var/lib/snapd/snaps
//	mount <name> <rev>                for each squashfs-ish mount under /snap
//	current <name> <target>           for each /snap/<name>/current symlink
//	data <name> <rev>                 for each /var/snap/<name>/<rev> dir
//	datacurrent <name> <target>       for each /var/snap/<name>/current symlink
//	unitfile <unit>                   for each snap mount unit file, loaded or not
const fsScript = `
cd /var/lib/snapd/snaps 2>/dev/null && for f in *.snap; do [ -e "$f" ] && echo "file $f"; done
awk '$2 ~ "^/snap/[^/]+/[^/]+$" {split($2,a,"/"); print "mount", a[3], a[4]}' /proc/self/mounts
for d in /snap/*/; do
  n=$(basename "$d"); [ "$n" = bin ] && continue
  [ -L "/snap/$n/current" ] && echo "current $n $(readlink /snap/$n/current)"
done
for d in /var/snap/*/; do
  n=$(basename "$d")
  for r in "$d"*/; do r=$(basename "$r"); case "$r" in common|current|"*") ;; *) echo "data $n $r";; esac; done
  [ -L "/var/snap/$n/current" ] && echo "datacurrent $n $(readlink /var/snap/$n/current)"
done
for f in /etc/systemd/system/snap-*.mount; do [ -e "$f" ] && echo "unitfile $(basename "$f")"; done
echo end
`

func run(w http.ResponseWriter, timeout time.Duration, name string, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(err.Error() + "\n"))
	}
	w.Write(out)
}

func main() {
	http.HandleFunc("/kill-snapd", func(w http.ResponseWriter, r *http.Request) {
		// SIGKILL snapd mid-whatever; snapd.socket restarts it on the next request.
		run(w, 20*time.Second, "incus", "exec", container, "--", "systemctl", "kill", "-s", "KILL", "snapd.service")
	})
	http.HandleFunc("/restart-snapd", func(w http.ResponseWriter, r *http.Request) {
		run(w, 60*time.Second, "incus", "exec", container, "--", "systemctl", "restart", "snapd.service")
	})
	http.HandleFunc("/reboot", func(w http.ResponseWriter, r *http.Request) {
		// Hard power-cycle of the whole system container.
		run(w, 120*time.Second, "incus", "restart", "--force", container)
	})
	http.HandleFunc("/fs", func(w http.ResponseWriter, r *http.Request) {
		run(w, 30*time.Second, "incus", "exec", container, "--", "sh", "-c", fsScript)
	})
	http.HandleFunc("/units", func(w http.ResponseWriter, r *http.Request) {
		run(w, 30*time.Second, "incus", "exec", container, "--", "sh", "-c",
			"systemctl list-units --all --plain --no-legend 'snap-*.mount' 'snap.*' | awk '{print $1, $2, $3, $4}'; echo end")
	})
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		run(w, 10*time.Second, "incus", "exec", container, "--", "true")
	})
	log.Fatal(http.ListenAndServe(":7001", nil))
}
