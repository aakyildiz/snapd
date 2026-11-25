// Command agent runs in the Incus host container. It gives the driver
// out-of-band control over the snapd system container: crash injection and
// a view of the filesystem that snapd manages. It never observes snapd's API.
package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"time"
)

const container = "snapd"

// forwarder is the start of the command line of start.sh's store forwarder.
const forwarder = "socat TCP-LISTEN:11029"

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
	// Reproduction controls (finding reproductions on snapd2; see
	// driver/repro.go). None of these is used by the main workload.
	http.HandleFunc("/sh", func(w http.ResponseWriter, r *http.Request) {
		// POST a shell script; it runs in the system container.
		b, _ := io.ReadAll(r.Body)
		run(w, 60*time.Second, "incus", "exec", container, "--", "sh", "-c", string(b))
	})
	http.HandleFunc("/fault", func(w http.ResponseWriter, r *http.Request) {
		// Restart snapd with SNAPD_FAULT_INJECT=<inject> (snapd is built
		// with the faultinject tag) and no fault stamps; empty clears it.
		inject := r.URL.Query().Get("inject")
		if !regexp.MustCompile(`^[a-z0-9:,-]*$`).MatchString(inject) {
			http.Error(w, "bad inject", http.StatusBadRequest)
			return
		}
		script := "rm -rf /var/lib/snapd/faults; mkdir -p /var/lib/snapd/environment; " +
			"printf 'SNAPPY_TESTING=1\\nSNAPD_FAULT_INJECT=" + inject + "\\n' > /var/lib/snapd/environment/snapd.conf; " +
			"systemctl reset-failed snapd.service snapd.socket; " +
			"for i in 1 2 3; do systemctl restart snapd.service && exit 0; sleep 2; systemctl reset-failed snapd.service; done; exit 1"
		run(w, 90*time.Second, "incus", "exec", container, "--", "sh", "-c", script)
	})
	http.HandleFunc("/systemctl-fail", func(w http.ResponseWriter, r *http.Request) {
		// While set, the systemctl shim (/usr/local/sbin/systemctl in the
		// container) fails calls whose arguments match this regexp.
		pat := r.URL.Query().Get("pattern")
		script := "rm -f /run/systemctl-fail"
		if pat != "" {
			script = "printf '%s\\n' \"$1\" > /run/systemctl-fail"
		}
		run(w, 20*time.Second, "incus", "exec", container, "--", "sh", "-c", script, "sh", pat)
	})
	http.HandleFunc("/store-down", func(w http.ResponseWriter, r *http.Request) {
		// Cut snapd off from the store: stop the host-side forwarder.
		exec.Command("pkill", "-f", "^"+forwarder).Run()
	})
	http.HandleFunc("/store-up", func(w http.ResponseWriter, r *http.Request) {
		if exec.Command("pgrep", "-f", "^"+forwarder).Run() == nil {
			return
		}
		cmd := exec.Command("socat", "TCP-LISTEN:11029,bind=127.0.0.1,fork,reuseaddr", "TCP:store:11028")
		if err := cmd.Start(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		go cmd.Wait()
	})
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		run(w, 10*time.Second, "incus", "exec", container, "--", "true")
	})
	log.Fatal(http.ListenAndServe(":7001", nil))
}
