package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for terraform/devbox/lifecycle/spot-watch.sh: design note 031,
// section 2.4, against a fake instance metadata service.

const fakeStart int64 = 1_700_000_000

func isoTime(epoch int64) string { return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z") }

// R-TEST: a notice from before this boot is stale even when it is less than
// two minutes old (a quick reboot after the event).
func TestSpotWatchNoticeFromBeforeAQuickReboot(t *testing.T) {
	b := newLifecycleBox(t)
	b.write("proc-stat", fmt.Sprintf("cpu  1 2 3\nbtime %d\n", fakeStart-30))
	b.imds.setNotices(fmt.Sprintf(`{"action": "stop", "time": %q}`, isoTime(fakeStart-60)), "")
	b.watchOnce()
	if b.count(gatewayStop) != 0 || b.logCount("stale interruption notice ignored") != 1 {
		t.Fatalf("calls %v log:\n%s", b.calls(), b.read(b.logFile))
	}
}

// R-TEST: the two-minute rule's edge: 90 s late is still acted on.
func TestSpotWatchLateButNotStaleNotice(t *testing.T) {
	b := newLifecycleBox(t)
	b.imds.setNotices(fmt.Sprintf(`{"action": "stop", "time": %q}`, isoTime(fakeStart-90)), "")
	b.watchOnce()
	if b.count(gatewayStop) != 1 {
		t.Fatalf("a notice 90 s late was not acted on: %v; log:\n%s", b.calls(), b.read(b.logFile))
	}
}

func (b *lifecycleBox) watchOnce() {
	b.t.Helper()
	out, status := b.run("spot-watch.sh", "--once")
	if status != 0 {
		b.t.Fatalf("spot-watch.sh exited %d:\n%s", status, out)
	}
	b.noTokenLeak(out)
}

func (b *lifecycleBox) count(call string) int {
	n := 0
	for _, c := range b.calls() {
		if c == call {
			n++
		}
	}
	return n
}

func (b *lifecycleBox) logCount(substring string) int {
	n := 0
	for _, line := range b.logLines() {
		if strings.Contains(line, substring) {
			n++
		}
	}
	return n
}

const (
	gatewayStop  = "systemctl stop ahp-gateway.service"
	gatewayStart = "systemctl start --no-block ahp-gateway.service"
)

func TestSpotWatchNoNotice(t *testing.T) {
	b := newLifecycleBox(t)
	for i := 0; i < 3; i++ {
		b.watchOnce()
		b.advance(5)
	}
	if b.count(gatewayStop) != 0 || len(b.hook.posts()) != 0 || len(b.logLines()) != 0 {
		t.Fatalf("did something without a notice: calls %v posts %v log %v", b.calls(), b.hook.posts(), b.logLines())
	}
	if b.imds.tokensIssued() != 1 {
		t.Fatalf("session tokens issued: %d, want 1 reused", b.imds.tokensIssued())
	}
}

func TestSpotWatchStopNotice(t *testing.T) {
	for _, action := range []string{"stop", "hibernate", "terminate"} {
		t.Run(action, func(t *testing.T) {
			b := newLifecycleBox(t)
			b.gateway.set(200, statusJSON(2, 1, 0))
			b.imds.setNotices(fmt.Sprintf(`{"action": %q, "time": %q}`, action, isoTime(fakeStart+120)), "")
			b.watchOnce()
			if b.count(gatewayStop) != 1 {
				t.Fatalf("gateway stops: %d; calls %v", b.count(gatewayStop), b.calls())
			}
			posts := b.hook.posts()
			if len(posts) != 1 || !strings.Contains(posts[0], "spot interruption: "+action) ||
				!strings.Contains(posts[0], "3 sessions, 1 turns running, 1 waiting") {
				t.Fatalf("posts: %q", posts)
			}
			if b.logCount("interruption notice: "+action) != 1 || b.logCount("gateway stopped in") != 1 {
				t.Fatalf("log:\n%s", b.read(b.logFile))
			}
			// Later polls, and a restarted watcher, do not repeat any of it.
			for i := 0; i < 3; i++ {
				b.advance(5)
				b.watchOnce()
			}
			if b.count(gatewayStop) != 1 || len(b.hook.posts()) != 1 || b.logCount("interruption notice") != 1 {
				t.Fatalf("repeated: calls %v posts %v log:\n%s", b.calls(), b.hook.posts(), b.read(b.logFile))
			}
		})
	}
}

func TestSpotWatchRetriesAStopThatDidNotFinish(t *testing.T) {
	b := newLifecycleBox(t)
	b.imds.setNotices(fmt.Sprintf(`{"action": "stop", "time": %q}`, isoTime(fakeStart+120)), "")
	b.write("stop-exit", "1\n")
	b.watchOnce()
	if b.logCount("did not stop") != 1 {
		t.Fatalf("log:\n%s", b.read(b.logFile))
	}
	b.remove("stop-exit")
	b.advance(5)
	b.watchOnce()
	if b.count(gatewayStop) != 2 || b.logCount("gateway stopped in") != 1 {
		t.Fatalf("stop not redone: calls %v log:\n%s", b.calls(), b.read(b.logFile))
	}
	if len(b.hook.posts()) != 1 {
		t.Fatalf("notice repeated: %q", b.hook.posts())
	}
}

func TestSpotWatchGatewayNotRunning(t *testing.T) {
	b := newLifecycleBox(t)
	b.write("gateway", "inactive\n")
	b.imds.setNotices(fmt.Sprintf(`{"action": "stop", "time": %q}`, isoTime(fakeStart+120)), "")
	b.watchOnce()
	if b.count(gatewayStop) != 0 || len(b.hook.posts()) != 1 {
		t.Fatalf("calls %v posts %v", b.calls(), b.hook.posts())
	}
	// It did not stop the gateway, so it never starts it.
	b.advance(600)
	b.watchOnce()
	if b.count(gatewayStart) != 0 {
		t.Fatal("started a gateway it had not stopped")
	}
}

func TestSpotWatchStaleNotice(t *testing.T) {
	for name, at := range map[string]int64{
		"before this boot":  fakeStart - 3600,
		"three minutes ago": fakeStart - 180,
	} {
		t.Run(name, func(t *testing.T) {
			b := newLifecycleBox(t)
			b.imds.setNotices(fmt.Sprintf(`{"action": "stop", "time": %q}`, isoTime(at)), "")
			for i := 0; i < 3; i++ {
				b.watchOnce()
				b.advance(5)
			}
			if b.count(gatewayStop) != 0 || len(b.hook.posts()) != 0 {
				t.Fatalf("acted on a stale notice: calls %v posts %v", b.calls(), b.hook.posts())
			}
			if b.logCount("stale interruption notice ignored") != 1 {
				t.Fatalf("log:\n%s", b.read(b.logFile))
			}
		})
	}
}

func TestSpotWatchInterruptionThatDidNotHappen(t *testing.T) {
	b := newLifecycleBox(t)
	at := fakeStart + 120
	b.imds.setNotices(fmt.Sprintf(`{"action": "stop", "time": %q}`, isoTime(at)), "")
	b.watchOnce()
	// The notice stays in the metadata the whole time.
	b.setNow(at + 299)
	b.watchOnce()
	if b.count(gatewayStart) != 0 {
		t.Fatal("started the gateway again too early")
	}
	b.setNow(at + 300)
	b.watchOnce()
	if b.count(gatewayStart) != 1 || b.logCount("interruption did not happen") != 1 {
		t.Fatalf("calls %v log:\n%s", b.calls(), b.read(b.logFile))
	}
	for i := 0; i < 3; i++ {
		b.advance(5)
		b.watchOnce()
	}
	if b.count(gatewayStart) != 1 || b.count(gatewayStop) != 1 {
		t.Fatalf("the same notice acted again: %v", b.calls())
	}
	if posts := b.hook.posts(); len(posts) != 2 || !strings.Contains(posts[1], "did not happen") {
		t.Fatalf("posts: %q", posts)
	}
}

func TestSpotWatchRebalance(t *testing.T) {
	b := newLifecycleBox(t)
	b.imds.setNotices("", fmt.Sprintf(`{"noticeTime": %q}`, isoTime(fakeStart)))
	for i := 0; i < 3; i++ {
		b.watchOnce()
		b.advance(5)
	}
	if b.count(gatewayStop) != 0 {
		t.Fatal("stopped the gateway on a rebalance recommendation")
	}
	if posts := b.hook.posts(); len(posts) != 1 || !strings.Contains(posts[0], "rebalance") {
		t.Fatalf("posts: %q", posts)
	}
	if b.logCount("rebalance recommendation") != 1 {
		t.Fatalf("log:\n%s", b.read(b.logFile))
	}
}

func TestSpotWatchMetadataUnreachable(t *testing.T) {
	b := newLifecycleBox(t)
	b.imds.server.Close()
	for i := 0; i < 4; i++ {
		b.watchOnce()
		b.advance(5)
	}
	if b.logCount("metadata unreachable") != 1 {
		t.Fatalf("log:\n%s", b.read(b.logFile))
	}
	// Back again, at a new address.
	b.imds = newFakeIMDS(t)
	for i, entry := range b.env {
		if strings.HasPrefix(entry, "DEVBOX_IMDS=") {
			b.env[i] = "DEVBOX_IMDS=" + b.imds.server.URL
		}
	}
	b.watchOnce()
	b.watchOnce()
	if b.logCount("metadata reachable again") != 1 {
		t.Fatalf("log:\n%s", b.read(b.logFile))
	}
}

func TestSpotWatchMalformedNotice(t *testing.T) {
	b := newLifecycleBox(t)
	b.imds.setNotices("not json", "")
	for i := 0; i < 3; i++ {
		b.watchOnce()
		b.advance(5)
	}
	if b.count(gatewayStop) != 0 || b.logCount("instance-action is not the expected JSON") != 1 {
		t.Fatalf("calls %v log:\n%s", b.calls(), b.read(b.logFile))
	}
}

func TestSpotWatchRenewsAnExpiredSessionToken(t *testing.T) {
	b := newLifecycleBox(t)
	b.watchOnce()
	b.imds.expireTokens()
	b.imds.setNotices(fmt.Sprintf(`{"action": "stop", "time": %q}`, isoTime(fakeStart+120)), "")
	b.watchOnce()
	if b.imds.tokensIssued() != 2 || b.count(gatewayStop) != 1 {
		t.Fatalf("tokens issued %d, calls %v; log:\n%s", b.imds.tokensIssued(), b.calls(), b.read(b.logFile))
	}
	if b.logCount("unreachable") != 0 {
		t.Fatalf("a renewal was logged as unreachable:\n%s", b.read(b.logFile))
	}
}

func TestSpotWatchRenewsATokenNearExpiry(t *testing.T) {
	b := newLifecycleBox(t)
	b.watchOnce()
	b.advance(21600 - 301)
	b.watchOnce()
	if b.imds.tokensIssued() != 1 {
		t.Fatalf("renewed early: %d", b.imds.tokensIssued())
	}
	b.advance(2)
	b.watchOnce()
	if b.imds.tokensIssued() != 2 {
		t.Fatalf("not renewed 5 min before expiry: %d", b.imds.tokensIssued())
	}
}

func TestSpotWatchSessionTokenFileIsPrivate(t *testing.T) {
	b := newLifecycleBox(t)
	b.watchOnce()
	if got := mode(t, filepath.Join(b.root, "spot", "token")); got != 0o600 {
		t.Fatalf("token file mode %o", got)
	}
	if got := mode(t, filepath.Join(b.root, "spot")); got != 0o700 {
		t.Fatalf("state dir mode %o", got)
	}
}

// The loop itself: a notice that appears while it runs stops the gateway
// within a few polls.
func TestSpotWatchLoop(t *testing.T) {
	b := newLifecycleBox(t)
	command := exec.Command("sh", lifecycleScript(t, "spot-watch.sh"))
	command.Env = append(b.env, "DEVBOX_SPOT_POLL_SECONDS=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		command.Process.Kill()
		command.Wait()
	})
	b.imds.setNotices(fmt.Sprintf(`{"action": "stop", "time": %q}`, isoTime(fakeStart+120)), "")
	deadline := time.Now().Add(20 * time.Second)
	for b.count(gatewayStop) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the loop did not stop the gateway; log:\n%s", b.read(b.logFile))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(b.root, "spot")); err != nil {
		t.Fatal(err)
	}
}

// Review finding: a watcher that died after stopping the gateway, before
// recording it, lost track that the gateway was its to start again.
func TestSpotWatchRemembersAStopThatWasNotRecorded(t *testing.T) {
	b := newLifecycleBox(t)
	at := fakeStart + 120
	b.imds.setNotices(fmt.Sprintf(`{"action": "stop", "time": %q}`, isoTime(at)), "")
	b.watchOnce()
	dir := filepath.Join(b.root, "spot", "notice-stop_"+strings.NewReplacer("-", "_", ":", "_").Replace(isoTime(at)))
	// As if the watcher died between the stop and writing `stopped`.
	if err := os.Remove(filepath.Join(dir, "stopped")); err != nil {
		t.Fatalf("no stopped marker to remove: %v", err)
	}
	b.watchOnce()
	b.setNow(at + 300)
	b.watchOnce()
	if b.count(gatewayStart) != 1 {
		t.Fatalf("the gateway it stopped is not started again: %v", b.calls())
	}
}

// Review finding: a notice not yet posted is posted even when the stop is
// already recorded.
func TestSpotWatchPostsANoticeLeftOut(t *testing.T) {
	b := newLifecycleBox(t)
	at := fakeStart + 120
	b.imds.setNotices(fmt.Sprintf(`{"action": "stop", "time": %q}`, isoTime(at)), "")
	b.watchOnce()
	dir := filepath.Join(b.root, "spot", "notice-stop_"+strings.NewReplacer("-", "_", ":", "_").Replace(isoTime(at)))
	if err := os.Remove(filepath.Join(dir, "noticed")); err != nil {
		t.Fatalf("no noticed marker: %v", err)
	}
	b.advance(5)
	b.watchOnce()
	if len(b.hook.posts()) != 2 {
		t.Fatalf("posts %q", b.hook.posts())
	}
	b.advance(5)
	b.watchOnce()
	if len(b.hook.posts()) != 2 {
		t.Fatalf("posted again: %q", b.hook.posts())
	}
}

// Review finding: after a hibernation the restart is right but the words
// "did not happen" were wrong.
func TestSpotWatchAfterHibernation(t *testing.T) {
	b := newLifecycleBox(t)
	at := fakeStart + 120
	b.imds.setNotices(fmt.Sprintf(`{"action": "hibernate", "time": %q}`, isoTime(at)), "")
	b.watchOnce()
	b.imds.setNotices("", "")
	b.setNow(at + 3600)
	b.watchOnce()
	if b.count(gatewayStart) != 1 {
		t.Fatalf("calls %v", b.calls())
	}
	posts := b.hook.posts()
	if strings.Contains(posts[len(posts)-1], "did not happen") || !strings.Contains(posts[len(posts)-1], "hibernat") {
		t.Fatalf("posts %q", posts)
	}
}
