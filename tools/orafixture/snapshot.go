// snapshot.go — live sidecar snapshotting for orafixture (UCL §U2): the
// oracle's -wal and live -journal images exist only while the session is
// open (clean close checkpoints/truncates the WAL per wal.c
// sqlite3WalClose, and PERSIST mode zeroes the journal header at commit
// per pager.c pager_end_transaction), so the scenario is replayed with the
// CLI's stdin held open and the sidecars copied mid-session.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// walReadyMarker terminates the scenario script; the oracle echoes it on
// stdout once all preceding statements (including the WAL writes) executed.
const walReadyMarker = "ORAFIXTURE_WAL_READY"

// liveSidecars lists the sidecar suffixes a scenario requests.
func liveSidecars(e entry) []string {
	var out []string
	if e.scenario.WalFiles {
		out = append(out, "-wal")
	}
	if e.scenario.JournalFiles {
		out = append(out, "-journal")
	}
	return out
}

// generateLiveSnapshot replays the scenario with the oracle's stdin held
// open, waits for the ready marker on stdout, then copies the requested
// sidecar files before letting the process exit. After the exit mutates
// the sidecars, the snapshots are restored so the fixtures hold the
// mid-session state.
func generateLiveSnapshot(e entry, dbPath string, sidecars []string) error {
	script, err := buildScript(e)
	if err != nil {
		return err
	}
	cmd := exec.Command(oracleCLI, dbPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	snaps, err := snapshotMidSession(cmd, stdin, stdout, script, dbPath, sidecars, &stderr)
	if err != nil {
		return err
	}
	if err := stdin.Close(); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("oracle exit: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	for p, snap := range snaps {
		if err := os.WriteFile(p, snap, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// snapshotMidSession feeds the script, waits for the ready marker, and
// reads the requested sidecars while the oracle session is alive. On
// failure the session is torn down before returning.
func snapshotMidSession(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader,
	script, dbPath string, sidecars []string, stderr *bytes.Buffer) (map[string][]byte, error) {
	fail := func(err error) (map[string][]byte, error) {
		stdin.Close()
		cmd.Wait()
		return nil, err
	}
	if _, err := io.WriteString(stdin, script+"SELECT '"+walReadyMarker+"';\n"); err != nil {
		return fail(fmt.Errorf("write script: %w", err))
	}
	if err := waitMarker(stdout, walReadyMarker); err != nil {
		return fail(fmt.Errorf("oracle session: %w: %s", err, strings.TrimSpace(stderr.String())))
	}
	snaps := make(map[string][]byte, len(sidecars))
	for _, sc := range sidecars {
		p := dbPath + sc
		if fi, err := os.Stat(p); err != nil || fi.Size() == 0 {
			return fail(fmt.Errorf("sidecar %s missing/empty mid-session (scenario needs the matching PRAGMA journal_mode)", p))
		}
		snap, err := os.ReadFile(p)
		if err != nil {
			return fail(err)
		}
		snaps[p] = snap
	}
	return snaps, nil
}

// waitMarker reads stdout until the marker line appears.
func waitMarker(r io.Reader, marker string) error {
	buf := make([]byte, 4096)
	var acc strings.Builder
	for !strings.Contains(acc.String(), marker) {
		n, err := r.Read(buf)
		acc.Write(buf[:n])
		if err != nil {
			return fmt.Errorf("marker %q not seen: %w", marker, err)
		}
	}
	return nil
}
