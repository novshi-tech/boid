package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// --- fixtures ---------------------------------------------------------------
//
// Both fixtures are real, not invented. The engines disagree on where they
// keep a container's per-container directory, and the whole point of parsing
// mountinfo rather than /proc/self/cgroup is that this is the ONE place both
// still spell the container id out.

// podmanMountInfo is a verbatim excerpt of `podman exec boid_daemon_1 cat
// /proc/self/mountinfo` taken from the live rootless-podman 4.9.3 dogfood
// deploy on 2026-07-26 (only lines were dropped, none edited). Note:
//
//   - the runtime-injected files come from
//     ".../overlay-containers/<id>/userdata/..." — NOT ".../containers/<id>/..."
//     the way docker spells it;
//   - the root field is relative to the tmpfs mount (0:47 = /run/user/1000),
//     so it starts "/containers/overlay-containers/..." and contains no
//     ".local/share" segment at all;
//   - line 960 shows the daemon state volume already resolved to a host path
//     — deliberately NOT what this parser keys off, because that shape is
//     podman-specific and carries no volume NAME on other engines.
const podmanMountInfo = `959 975 252:0 /usr/bin/catatonit /run/podman-init ro,relatime - ext4 /dev/mapper/ubuntu--vg-ubuntu--lv rw
960 975 252:0 /home/nosen/.local/share/containers/storage/volumes/boid_boid_state/_data /home/boid rw,nosuid,nodev,relatime - ext4 /dev/mapper/ubuntu--vg-ubuntu--lv rw
961 975 0:47 /containers/overlay-containers/b94552dff710bd13f2aad818272998c2679f6b76d4ee0aec26a979602dfe5e9f/userdata/resolv.conf /etc/resolv.conf rw,nosuid,nodev,relatime - tmpfs tmpfs rw,size=6257324k,nr_inodes=1564331,mode=700,uid=1000,gid=1000,inode64
962 975 0:47 /containers/overlay-containers/b94552dff710bd13f2aad818272998c2679f6b76d4ee0aec26a979602dfe5e9f/userdata/hosts /etc/hosts rw,nosuid,nodev,relatime - tmpfs tmpfs rw,size=6257324k,nr_inodes=1564331,mode=700,uid=1000,gid=1000,inode64
964 975 0:47 /containers/overlay-containers/b94552dff710bd13f2aad818272998c2679f6b76d4ee0aec26a979602dfe5e9f/userdata/.containerenv /run/.containerenv rw,nosuid,nodev,relatime - tmpfs tmpfs rw,size=6257324k,nr_inodes=1564331,mode=700,uid=1000,gid=1000,inode64
965 975 0:47 /containers/overlay-containers/b94552dff710bd13f2aad818272998c2679f6b76d4ee0aec26a979602dfe5e9f/userdata/hostname /etc/hostname rw,nosuid,nodev,relatime - tmpfs tmpfs rw,size=6257324k,nr_inodes=1564331,mode=700,uid=1000,gid=1000,inode64
966 975 0:47 /podman/podman.sock /run/docker.sock rw,nosuid,nodev,relatime - tmpfs tmpfs rw,size=6257324k,nr_inodes=1564331,mode=700,uid=1000,gid=1000,inode64
975 108 0:130 / / rw,relatime - overlay overlay rw,lowerdir=/home/nosen/.local/share/containers/storage/overlay/l/E4V4Q3677ZE2P7YXBDVQWPAOBX,upperdir=/home/nosen/.local/share/containers/storage/overlay/b05ae1b6c6034345ae26c4e14fcee2f14f51d6557154ed0d646bfecedbba0f28/diff,workdir=/home/nosen/.local/share/containers/storage/overlay/b05ae1b6c6034345ae26c4e14fcee2f14f51d6557154ed0d646bfecedbba0f28/work,redirect_dir=nofollow,uuid=on,userxattr
976 975 0:133 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
981 977 0:29 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup2 rw,nsdelegate,memory_recursiveprot
`

const podmanContainerID = "b94552dff710bd13f2aad818272998c2679f6b76d4ee0aec26a979602dfe5e9f"

// dockerMountInfo is lifted from moby's own test corpus
// (github.com/moby/moby@v28.5.2+incompatible daemon/daemon_linux_test.go,
// the mountinfo sample its parser tests run against), which is the closest
// thing to an authoritative statement of the shape docker produces. The
// discriminating lines are the three runtime-injected files rooted at
// "/var/lib/docker/containers/<id>/". The cgroup-v1 lines are kept because
// they carry the SAME id under a different shape ("/docker/<id>") and would
// be a tempting-but-wrong thing for a parser to key off (they are absent
// under cgroup v2 — see cgroupV2Unhelpful below).
const dockerMountInfo = `142 92 8:4 /var/lib/docker/aufs/mnt/x / rw,relatime - aufs none rw,si=573b861da0b22a5b,dio
143 142 0:60 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
149 148 0:22 /docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a /sys/fs/cgroup/cpuset rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,cpuset
160 142 8:4 /var/lib/docker/volumes/9a428b651ee4c538130143cad8d87f603a4bf31b928afe7ff3ecd65480692b35/_data /var/lib/docker rw,relatime - ext4 /dev/disk/by-uuid/d99e196c rw,errors=remount-ro,data=ordered
165 142 8:4 /var/lib/docker/containers/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/disk/by-uuid/d99e196c rw,errors=remount-ro,data=ordered
166 142 8:4 /var/lib/docker/containers/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a/hostname /etc/hostname rw,relatime - ext4 /dev/disk/by-uuid/d99e196c rw,errors=remount-ro,data=ordered
167 142 8:4 /var/lib/docker/containers/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a/hosts /etc/hosts rw,relatime - ext4 /dev/disk/by-uuid/d99e196c rw,errors=remount-ro,data=ordered
116 160 0:107 / /var/lib/docker/containers/d045dc441d2e2e1d5b3e328d47e5943811a40819fb47497c5f5a5df2d6d13c37/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k
`

const dockerContainerID = "5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a"

// hostMountInfo is the head of this developer host's own /proc/self/mountinfo
// (verbatim, 2026-07-26) — no container anywhere in it, and every path a
// daemon would keep state under (/home/..., /var/lib/...) falls on the ROOT
// mount. It is the "bare `boid start`" case that must stay silent.
const hostMountInfo = `24 30 0:22 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs sysfs rw
25 30 0:23 / /proc rw,nosuid,nodev,noexec,relatime shared:13 - proc proc rw
28 30 0:25 / /run rw,nosuid,nodev,noexec,relatime shared:5 - tmpfs tmpfs rw,size=6257328k,mode=755,inode64
30 1 252:0 / / rw,relatime shared:1 - ext4 /dev/mapper/ubuntu--vg-ubuntu--lv rw
43 30 259:2 / /boot rw,relatime shared:30 - ext4 /dev/nvme0n1p2 rw
`

// k8sMountInfo is the environment [Major 1] was reported against and the whole
// reason the "am I in a container?" question had to be re-asked: a container
// with NO engine sentinel file (containerd/CRI writes neither /.dockerenv nor
// /run/.containerenv), NO container id anywhere in mountinfo (the pod's
// injected files are rooted at a kubelet path carrying a 36-char pod UUID, not
// a 64-hex container id) and cgroup v2 reporting just "0::/". The daemon's
// state, however, is unmistakably on a mount of its own: /home/boid is a
// separate mount, exactly as it is under compose.
//
// Under the sentinel-based rule this environment classified as "not in a
// container", so detection returned success with an empty reservation and the
// daemon started SILENTLY undefended — on a cluster whose nodes expose a
// docker-compatible socket, the sibling-mount attack lands unchanged.
const k8sMountInfo = `1234 1200 0:74 / / rw,relatime - overlay overlay rw,lowerdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/821/fs
1235 1234 0:75 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
1240 1234 259:1 /var/lib/kubelet/pods/2f8a1c3e-6b71-4a0e-9f52-1d6c0a7b3e11/volumes/kubernetes.io~csi/boid-state/mount /home/boid rw,relatime - ext4 /dev/nvme0n1p1 rw
1241 1234 259:1 /var/lib/kubelet/pods/2f8a1c3e-6b71-4a0e-9f52-1d6c0a7b3e11/etc-hosts /etc/hosts rw,relatime - ext4 /dev/nvme0n1p1 rw
1242 1234 259:1 /var/lib/kubelet/pods/2f8a1c3e-6b71-4a0e-9f52-1d6c0a7b3e11/containers/boid/8f2c /dev/termination-log rw,relatime - ext4 /dev/nvme0n1p1 rw
`

// separateHomePartitionMountInfo is a bare host whose /home is its own
// filesystem — a very common Linux install. The daemon's state is on a mount
// of its own here too, and no container id exists to find, so the mount-based
// rule classifies it as "should have found a volume, did not" and WARNs. That
// is a deliberate FALSE POSITIVE: one noisy line at startup on a deployment
// that had nothing to protect, versus silence on the k8s shape above, which
// had everything to protect. The direction is the point.
const separateHomePartitionMountInfo = `30 1 252:0 / / rw,relatime shared:1 - ext4 /dev/mapper/ubuntu--vg-ubuntu--lv rw
25 30 0:23 / /proc rw,nosuid,nodev,noexec,relatime shared:13 - proc proc rw
44 30 259:3 / /home rw,relatime shared:31 - ext4 /dev/nvme0n1p3 rw
`

// cgroupV2Unhelpful is verbatim `podman exec boid_daemon_1 cat
// /proc/self/cgroup` from the same live deploy. It is the entire reason
// mountinfo is the primary source: under cgroup v2 the daemon's own cgroup
// file says nothing at all.
const cgroupV2Unhelpful = "0::/\n"

const cgroupV1Docker = `12:pids:/docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a
11:memory:/docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a
`

const cgroupV2PodmanScope = "0::/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-" +
	podmanContainerID + ".scope/container\n"

// cgroupNestedDocker is docker-in-docker: the cgroup path carries the OUTER
// container's id first and this process's own id second. [Major 3] — a
// fallback that returns the first id it sees inspects the PARENT, and if the
// parent happens to have a /home/boid volume of its own the daemon reports
// "containment active, DataHomeCovered=true" while reserving a name that has
// nothing to do with the volume its own secrets are in. There is no way to
// tell from this file alone which of the two is "me", so the only honest
// answer is to refuse it.
const cgroupNestedDocker = "0::/docker/" + dockerContainerID + "/docker/" + podmanContainerID + "\n"

// nestedMountInfo is the same ambiguity on the primary source: a
// runtime-injected mount whose root carries two 64-hex segments. Synthetic
// (unlike every other mountinfo fixture here) — it stands for "a layout this
// parser does not understand", and the contract it pins is that an
// un-understood layout is refused rather than guessed at.
const nestedMountInfo = `142 92 8:4 /var/lib/docker/aufs/mnt/x / rw,relatime - aufs none rw
165 142 8:4 /var/lib/docker/containers/` + dockerContainerID + `/containers/` + podmanContainerID + `/hostname /etc/hostname rw,relatime - ext4 /dev/disk/by-uuid/d99e196c rw
`

// --- helpers ----------------------------------------------------------------

// installProcFixtures points the /proc seams at literal contents, restoring
// them afterwards. There is deliberately no "am I in a container" knob: the
// classification is derived from the mountinfo fixture itself (see
// daemonStateMayLiveOnVolume), which is the whole of [Major 1]'s fix.
func installProcFixtures(t *testing.T, mountInfo, cgroup string) {
	t.Helper()
	origMount, origCgroup := readSelfMountInfo, readSelfCgroup
	t.Cleanup(func() {
		readSelfMountInfo, readSelfCgroup = origMount, origCgroup
	})

	readSelfMountInfo = func() ([]byte, error) { return []byte(mountInfo), nil }
	readSelfCgroup = func() ([]byte, error) { return []byte(cgroup), nil }
}

type stubInspector struct {
	gotID  string
	mounts []container.MountPoint
	err    error
}

func (s *stubInspector) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	s.gotID = id
	if s.err != nil {
		return client.ContainerInspectResult{}, s.err
	}
	return client.ContainerInspectResult{
		Container: container.InspectResponse{ID: id, Mounts: s.mounts},
	}, nil
}

// composeDaemonMounts is the live deploy's actual mount set, from
// `podman inspect boid_daemon_1 --format '{{json .Mounts}}'` on 2026-07-26:
// one named volume plus two binds (the engine socket and BOID_RUNTIME_DIR).
func composeDaemonMounts() []container.MountPoint {
	return []container.MountPoint{
		{Type: mount.TypeVolume, Name: "boid_boid_state", Destination: "/home/boid",
			Source: "/home/nosen/.local/share/containers/storage/volumes/boid_boid_state/_data"},
		{Type: mount.TypeBind, Source: "/run/user/1000/podman/podman.sock", Destination: "/var/run/docker.sock"},
		{Type: mount.TypeBind, Source: "/run/user/1000", Destination: "/run/user/1000"},
	}
}

// --- container id extraction ------------------------------------------------

func TestSelfContainerID_fromMountInfo(t *testing.T) {
	cases := []struct {
		name      string
		mountInfo string
		cgroup    string
		want      string
	}{
		{"podman rootless (live dogfood deploy)", podmanMountInfo, cgroupV2Unhelpful, podmanContainerID},
		{"docker (moby's own mountinfo corpus)", dockerMountInfo, cgroupV2Unhelpful, dockerContainerID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installProcFixtures(t, tc.mountInfo, tc.cgroup)
			got, err := selfContainerID()
			if err != nil {
				t.Fatalf("selfContainerID: %v", err)
			}
			if got != tc.want {
				t.Errorf("selfContainerID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSelfContainerID_ignoresOtherContainersInMountInfo pins the reason the
// parser keys off the runtime-injected DESTINATIONS rather than "any 64-hex
// segment anywhere": the docker fixture also contains
// /var/lib/docker/containers/d045dc44.../shm — a DIFFERENT container's
// directory, appearing as a mount POINT. Picking that up would make the
// daemon inspect (and then reserve the volumes of) a container that is not
// itself.
func TestSelfContainerID_ignoresOtherContainersInMountInfo(t *testing.T) {
	installProcFixtures(t, dockerMountInfo, cgroupV2Unhelpful)
	got, err := selfContainerID()
	if err != nil {
		t.Fatalf("selfContainerID: %v", err)
	}
	if got == "d045dc441d2e2e1d5b3e328d47e5943811a40819fb47497c5f5a5df2d6d13c37" {
		t.Fatal("selfContainerID picked up another container's id from a mount POINT")
	}
	if got != dockerContainerID {
		t.Errorf("selfContainerID() = %q, want %q", got, dockerContainerID)
	}
}

func TestSelfContainerID_cgroupFallback(t *testing.T) {
	cases := []struct {
		name   string
		cgroup string
		want   string
	}{
		{"cgroup v1 docker", cgroupV1Docker, dockerContainerID},
		{"cgroup v2 libpod scope", cgroupV2PodmanScope, podmanContainerID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// mountinfo deliberately useless: the fallback is the thing under test.
			installProcFixtures(t, hostMountInfo, tc.cgroup)
			got, err := selfContainerID()
			if err != nil {
				t.Fatalf("selfContainerID: %v", err)
			}
			if got != tc.want {
				t.Errorf("selfContainerID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSelfContainerID_notInContainer(t *testing.T) {
	installProcFixtures(t, hostMountInfo, cgroupV2Unhelpful)
	if got, err := selfContainerID(); err == nil {
		t.Errorf("selfContainerID() = %q, want an error on a plain host", got)
	}
}

// --- DetectDaemonStateVolumes ----------------------------------------------

// TestDetectDaemonStateVolumes_liveComposeShape is the whole feature against
// the real deploy's data: podman mountinfo in, "boid_boid_state" out.
//
// Note on the dataHome it passes: /home/boid exists inside the daemon
// container and on no developer machine, so on the machine running this test
// the path resolution fails and the classification lands on "may live on a
// volume" for that reason rather than because /home/boid is its own mount.
// Both routes reach the same answer here (deliberately — see
// resolveDataHomeForMountComparison, which fails toward the noisy side), and
// the mount-based route against this exact fixture and this exact path is
// asserted directly by TestMountPointCovering_liveFixtures.
func TestDetectDaemonStateVolumes_liveComposeShape(t *testing.T) {
	installProcFixtures(t, podmanMountInfo, cgroupV2Unhelpful)
	api := &stubInspector{mounts: composeDaemonMounts()}

	res, err := DetectDaemonStateVolumes(context.Background(), api, "/home/boid/.local/share/boid")
	if err != nil {
		t.Fatalf("DetectDaemonStateVolumes: %v", err)
	}
	if !res.StateVolumeExpected {
		t.Error("StateVolumeExpected = false, want true")
	}
	if api.gotID != podmanContainerID {
		t.Errorf("inspected %q, want the daemon's own container %q", api.gotID, podmanContainerID)
	}
	if len(res.Names) != 1 || res.Names[0] != "boid_boid_state" {
		t.Errorf("Names = %v, want [boid_boid_state] (binds must not appear — they carry a host path, not a volume name)", res.Names)
	}
	if !res.DataHomeCovered {
		t.Error("DataHomeCovered = false, want true: /home/boid contains the daemon's data root")
	}
}

// The "silent no-op" and "failed to protect" halves of the contract live in
// the [Major 1] block near the bottom of this file, where the fixtures that
// discriminate them are explained.

func TestDetectDaemonStateVolumes_inspectFails(t *testing.T) {
	installProcFixtures(t, podmanMountInfo, cgroupV2Unhelpful)
	api := &stubInspector{err: errors.New("engine unreachable")}

	res, err := DetectDaemonStateVolumes(context.Background(), api, "/home/boid/.local/share/boid")
	if err == nil {
		t.Fatal("want an error when ContainerInspect fails")
	}
	if !strings.Contains(err.Error(), "engine unreachable") {
		t.Errorf("error %q does not carry the engine's own message", err)
	}
	if !res.StateVolumeExpected {
		t.Error("StateVolumeExpected = false, want true")
	}
}

// TestDetectDaemonStateVolumes_dataHomeOutsideEveryVolume pins the diagnostic
// that tells an operator name-based reservation is not covering the crown
// jewels — e.g. a deployment that keeps boid.db on a bind mount and has some
// unrelated named volume attached.
func TestDetectDaemonStateVolumes_dataHomeOutsideEveryVolume(t *testing.T) {
	installProcFixtures(t, podmanMountInfo, cgroupV2Unhelpful)
	api := &stubInspector{mounts: []container.MountPoint{
		{Type: mount.TypeVolume, Name: "some_cache", Destination: "/var/cache"},
		{Type: mount.TypeBind, Source: "/srv/boid-state", Destination: "/home/boid"},
	}}

	res, err := DetectDaemonStateVolumes(context.Background(), api, "/home/boid/.local/share/boid")
	if err != nil {
		t.Fatalf("DetectDaemonStateVolumes: %v", err)
	}
	if len(res.Names) != 1 || res.Names[0] != "some_cache" {
		t.Errorf("Names = %v, want [some_cache]", res.Names)
	}
	if res.DataHomeCovered {
		t.Error("DataHomeCovered = true, but the data root is on a bind mount, not on any named volume")
	}
}

// TestDetectDaemonStateVolumes_multipleVolumesAllReserved pins the design
// decision recorded in DetectDaemonStateVolumes' doc comment: EVERY named
// volume mounted into the daemon is reserved, not just the one holding
// DataHomeDir. A second daemon-owned volume (a future split, or an operator's
// own addition) must be protected the day it is added, without another PR.
func TestDetectDaemonStateVolumes_multipleVolumesAllReserved(t *testing.T) {
	installProcFixtures(t, podmanMountInfo, cgroupV2Unhelpful)
	api := &stubInspector{mounts: []container.MountPoint{
		{Type: mount.TypeVolume, Name: "boid_boid_state", Destination: "/home/boid"},
		{Type: mount.TypeVolume, Name: "boid_boid_repos", Destination: "/srv/repos"},
		{Type: mount.TypeBind, Source: "/run/user/1000", Destination: "/run/user/1000"},
	}}

	res, err := DetectDaemonStateVolumes(context.Background(), api, "/home/boid/.local/share/boid")
	if err != nil {
		t.Fatalf("DetectDaemonStateVolumes: %v", err)
	}
	want := []string{"boid_boid_repos", "boid_boid_state"} // sorted: stable log/test output
	if len(res.Names) != len(want) {
		t.Fatalf("Names = %v, want %v", res.Names, want)
	}
	for i := range want {
		if res.Names[i] != want[i] {
			t.Fatalf("Names = %v, want %v", res.Names, want)
		}
	}
}

// --- [Major 1] the "is there anything to protect?" axis ---------------------
//
// These four pin the replacement for the sentinel-file test. The question is
// no longer "is this an engine I recognize?" but "does this daemon's own
// persistent state sit on a mount of its own?" — a property of the ASSET
// being defended, which is what the reservation is actually about.

// TestDetectDaemonStateVolumes_stateOnOwnMountButUnidentifiable is the
// regression test for [Major 1]. Nothing here says "docker" or "podman": no
// sentinel file, no id in mountinfo, cgroup v2 saying "0::/". The state is
// none the less on a mount of its own, so identification FAILED and the caller
// must be told — the old rule returned success with an empty set and the
// daemon ran undefended without a word.
func TestDetectDaemonStateVolumes_stateOnOwnMountButUnidentifiable(t *testing.T) {
	// The verbatim k8s fixture supplies the thing under test — a mount table
	// with no extractable container id — while the appended line puts the data
	// root on a mount of its own at a path that exists here, so the
	// classification is real rather than an artifact of an unresolvable path.
	stateMount := filepath.Join(realTempDir(t), "state")
	dataHome := filepath.Join(stateMount, ".local", "share", "boid")
	if err := os.MkdirAll(dataHome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	installProcFixtures(t, k8sMountInfo+mountLines(stateMount), cgroupV2Unhelpful)
	api := &stubInspector{mounts: composeDaemonMounts()}

	res, err := DetectDaemonStateVolumes(context.Background(), api, dataHome)
	if err == nil {
		t.Fatal("want an error: the data root is on its own mount, so a volume may hold it, and no volume name could be produced")
	}
	if !res.StateVolumeExpected {
		t.Error("StateVolumeExpected = false; the caller cannot tell 'nothing to protect' from 'failed to protect', so it will not warn")
	}
	if len(res.Names) != 0 {
		t.Errorf("Names = %v, want none", res.Names)
	}
}

// TestMountPointCovering_liveFixtures is where the four verbatim mountinfo
// fixtures above pay for themselves: it asks, of each real mount table, which
// mount a daemon data root under it actually lands on. It is a pure-function
// test on purpose — mountPointCovering touches no filesystem, so the observed
// mount tables can be asserted against the paths they were observed WITH
// (/home/boid inside the container, /home/nosen on the dev host) without
// either path having to exist on the machine running the tests.
//
// The end-to-end classification is exercised separately, against real
// directories, by the two tests below. The split exists because
// DetectDaemonStateVolumes now resolves its dataHome against the real
// filesystem (resolveDataHomeForMountComparison), which a fixture path cannot
// satisfy — see [round 2 Major 1].
func TestMountPointCovering_liveFixtures(t *testing.T) {
	cases := []struct {
		name      string
		mountInfo string
		dataHome  string
		want      string
	}{
		{"live rootless podman daemon container", podmanMountInfo, "/home/boid/.local/share/boid", "/home/boid"},
		{"dev host, bare boid start", hostMountInfo, "/home/nosen/.local/share/boid", "/"},
		{"k8s CSI-mounted state", k8sMountInfo, "/home/boid/.local/share/boid", "/home/boid"},
		{"bare host with /home on its own filesystem", separateHomePartitionMountInfo, "/home/nosen/.local/share/boid", "/home"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := mountPointCovering(tc.mountInfo, tc.dataHome)
			if !ok {
				t.Fatalf("no mount covers %s", tc.dataHome)
			}
			if got != tc.want {
				t.Errorf("mountPointCovering(%s) = %q, want %q", tc.dataHome, got, tc.want)
			}
		})
	}
}

// TestDetectDaemonStateVolumes_bareHostStateOnRootMount is the other side:
// the data root is an ordinary directory on the root mount, so no named volume
// can be holding it and there is nothing to reserve or to warn about.
//
// A real directory with a generated root-only mount table, rather than the
// hostMountInfo constant with /home/nosen: the data root now has to resolve on
// the machine running the test, and this repo's tests must not depend on one
// developer's home directory existing. hostMountInfo's own shape is asserted
// by TestMountPointCovering_liveFixtures above.
func TestDetectDaemonStateVolumes_bareHostStateOnRootMount(t *testing.T) {
	dataHome := filepath.Join(realTempDir(t), "boid")
	if err := os.MkdirAll(dataHome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	installProcFixtures(t, mountInfoFixture(""), cgroupV2Unhelpful)
	api := &stubInspector{err: errors.New("must not be called")}

	res, err := DetectDaemonStateVolumes(context.Background(), api, dataHome)
	if err != nil {
		t.Fatalf("DetectDaemonStateVolumes on a plain host: %v", err)
	}
	if res.StateVolumeExpected {
		t.Error("StateVolumeExpected = true on a host where the data root is on the root mount")
	}
	if api.gotID != "" {
		t.Errorf("inspected %q; the engine must not be consulted when there is no volume to find", api.gotID)
	}
}

// TestDetectDaemonStateVolumes_separateHomePartitionWarns documents the known
// false positive of the mount-based rule, so a future reader does not "fix" it
// by reintroducing the sentinel. See separateHomePartitionMountInfo, whose own
// shape is asserted by TestMountPointCovering_liveFixtures; this test rebuilds
// that shape over a real directory so the whole function, resolution included,
// is exercised.
func TestDetectDaemonStateVolumes_separateHomePartitionWarns(t *testing.T) {
	homePartition := filepath.Join(realTempDir(t), "home")
	dataHome := filepath.Join(homePartition, "nosen", ".local", "share", "boid")
	if err := os.MkdirAll(dataHome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// No container id anywhere: a bare host, exactly like the real one.
	installProcFixtures(t, mountInfoFixture("", homePartition), cgroupV2Unhelpful)
	api := &stubInspector{mounts: composeDaemonMounts()}

	res, err := DetectDaemonStateVolumes(context.Background(), api, dataHome)
	if err == nil {
		t.Fatal("want an error: /home is its own mount here, so the rule cannot rule out a volume and must not go silent")
	}
	if !res.StateVolumeExpected {
		t.Error("StateVolumeExpected = false; a bare host with /home on its own filesystem must land on the noisy side, not the silent one")
	}
}

// TestDetectDaemonStateVolumes_undecidableFailsTowardWarning pins the tie
// break: when the question cannot be answered at all, answer "there might be
// something to protect". Silence is only ever correct as a POSITIVE finding.
func TestDetectDaemonStateVolumes_undecidableFailsTowardWarning(t *testing.T) {
	t.Run("mountinfo unreadable", func(t *testing.T) {
		// A data root that DOES resolve, so the only thing left undecidable is
		// the mount table itself — otherwise this would pass on the resolution
		// failure and never exercise the read error at all.
		dataHome := realTempDir(t)
		installProcFixtures(t, hostMountInfo, cgroupV2Unhelpful)
		readSelfMountInfo = func() ([]byte, error) { return nil, errors.New("permission denied") }

		res, err := DetectDaemonStateVolumes(context.Background(), &stubInspector{}, dataHome)
		if err == nil {
			t.Fatal("want an error when /proc/self/mountinfo cannot be read")
		}
		if !res.StateVolumeExpected {
			t.Error("StateVolumeExpected = false after a read failure; an unanswerable question must not present as 'nothing to protect'")
		}
	})

	t.Run("no data root configured", func(t *testing.T) {
		installProcFixtures(t, hostMountInfo, cgroupV2Unhelpful)

		res, err := DetectDaemonStateVolumes(context.Background(), &stubInspector{}, "")
		if err == nil {
			t.Fatal("want an error when there is no data root to locate")
		}
		if !res.StateVolumeExpected {
			t.Error("StateVolumeExpected = false with an unknown data root; the check did not run, so it cannot report 'nothing to protect'")
		}
	})
}

// --- [Major 2] the self-inspect must not be able to hang startup ------------

// TestDetectDaemonStateVolumes_inspectHangsHitsDeadline pins that a socket
// which accepts the connection and then never answers costs the daemon a
// bounded delay and a warning, not its startup. Without a deadline the engine
// call inherits context.Background() and `boid start` blocks forever.
func TestDetectDaemonStateVolumes_inspectHangsHitsDeadline(t *testing.T) {
	installProcFixtures(t, podmanMountInfo, cgroupV2Unhelpful)
	withSelfInspectTimeout(t, 200*time.Millisecond)

	api := &hangingInspector{}
	start := time.Now()
	res, err := DetectDaemonStateVolumes(context.Background(), api, "/home/boid/.local/share/boid")
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("DetectDaemonStateVolumes took %v against an engine that never answers; startup would hang", elapsed)
	}
	if err == nil {
		t.Fatal("want an error when the engine never answers")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
	}
	if !res.StateVolumeExpected {
		t.Error("StateVolumeExpected = false; a timeout is 'failed to protect', which must warn")
	}
	if len(res.Names) != 0 {
		t.Errorf("Names = %v, want none after a timeout", res.Names)
	}
}

// --- [Major 3] ambiguity must never be resolved by guessing -----------------

// TestSelfContainerID_cgroupNestedIDsRefusesToGuess is the regression test for
// [Major 3]: docker-in-docker puts the parent's id in front of ours, and a
// first-match fallback inspects the PARENT. If the parent also has a volume
// mounted at the same place, the daemon reports containment as ACTIVE while
// reserving a name that protects nothing of its own — worse than reserving
// nothing, because the warning that would have told an operator never fires.
func TestSelfContainerID_cgroupNestedIDsRefusesToGuess(t *testing.T) {
	// mountinfo deliberately carries no id: the cgroup fallback is under test.
	installProcFixtures(t, hostMountInfo, cgroupNestedDocker)

	got, err := selfContainerID()
	if err == nil {
		t.Fatalf("selfContainerID() = %q, want an error: /proc/self/cgroup names two containers and neither can be shown to be this one", got)
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error %q does not say the id was ambiguous", err)
	}
}

// TestSelfContainerID_mountInfoNestedIDsRefusesToGuess is the same rule on the
// primary source. A single line naming two containers is a layout this parser
// does not model, and "pick one" is exactly the failure mode [Major 3] is
// about.
func TestSelfContainerID_mountInfoNestedIDsRefusesToGuess(t *testing.T) {
	installProcFixtures(t, nestedMountInfo, cgroupV2Unhelpful)

	got, err := selfContainerID()
	if err == nil {
		t.Fatalf("selfContainerID() = %q, want an error: the mount root names two containers", got)
	}
}

// TestSelfContainerID_repeatedSameIDIsNotAmbiguous keeps the ambiguity rule
// from over-firing: cgroup v1 writes one line PER CONTROLLER, all carrying the
// same id, and mountinfo carries the same id on three injected files. Distinct
// ids are the trigger, not repeated ones.
func TestSelfContainerID_repeatedSameIDIsNotAmbiguous(t *testing.T) {
	installProcFixtures(t, hostMountInfo, cgroupV1Docker)
	got, err := selfContainerID()
	if err != nil {
		t.Fatalf("selfContainerID with one id repeated across cgroup controllers: %v", err)
	}
	if got != dockerContainerID {
		t.Errorf("selfContainerID() = %q, want %q", got, dockerContainerID)
	}
}

// --- [round 2 Major 1] the dataHome SPELLING must not decide the outcome ----
//
// The mount comparison is lexical, so whatever string reaches it has to be the
// same shape as the kernel's own fully-resolved mount points. Two spellings are
// not, and both used to classify as "covered by /" — i.e. "an ordinary
// directory on the root filesystem, nothing to protect" — and returned a
// successful EMPTY reservation with no warning at all. That is the exact
// failure shape [Major 1] (round 1) was about, reached by a different door.
//
// These use REAL directories and a mountinfo fixture generated from them
// (mountInfoFixture) rather than the hand-written constants above, because the
// property under test is precisely that the path resolution and the fixture
// agree — a fixture naming a path that does not exist could not tell a
// resolution success from a resolution failure.

// TestDetectDaemonStateVolumes_relativeDataHomeIsNotTreatedAsRootMounted pins
// the relative-path half. cmd/start.go's --db-path neither rejects nor
// absolutizes a relative path, so `boid start --db-path
// ../home/boid/.local/share/boid/boid.db` from /workspace puts the DB on the
// state volume while handing this function a string that lexically matches
// nothing but "/".
func TestDetectDaemonStateVolumes_relativeDataHomeIsNotTreatedAsRootMounted(t *testing.T) {
	root := realTempDir(t)
	volume := filepath.Join(root, "vol")
	state := filepath.Join(volume, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	installProcFixtures(t, mountInfoFixture(podmanContainerID, volume), cgroupV2Unhelpful)
	t.Chdir(root)

	api := &stubInspector{mounts: []container.MountPoint{
		{Type: mount.TypeVolume, Name: "boid_boid_state", Destination: volume},
	}}

	res, err := DetectDaemonStateVolumes(context.Background(), api, filepath.Join("vol", "state"))
	if err != nil {
		t.Fatalf("DetectDaemonStateVolumes: %v", err)
	}
	if !res.StateVolumeExpected {
		t.Fatal("StateVolumeExpected = false for a RELATIVE data root that resolves onto a mount of its own: " +
			"the caller reads that as 'nothing to protect' and stays silent, so the state volume stays mountable")
	}
	if len(res.Names) != 1 || res.Names[0] != "boid_boid_state" {
		t.Errorf("Names = %v, want [boid_boid_state]", res.Names)
	}
	if !res.DataHomeCovered {
		t.Error("DataHomeCovered = false; the relative data root does resolve inside the reserved volume")
	}
}

// TestDetectDaemonStateVolumes_symlinkedDataHomeResolvesToItsRealMount pins the
// symlink half — the same silent miss through an ancestor symlink, which round
// 1 knowingly left open on the grounds that filepath.EvalSymlinks could block
// under a hung filesystem. That premise does not survive contact: the daemon
// opens boid.db at this very path before this function is ever called, so a
// data root under a wedged mount has already stopped startup with or without
// the resolution.
func TestDetectDaemonStateVolumes_symlinkedDataHomeResolvesToItsRealMount(t *testing.T) {
	root := realTempDir(t)
	volume := filepath.Join(root, "vol")
	state := filepath.Join(volume, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(volume, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	installProcFixtures(t, mountInfoFixture(podmanContainerID, volume), cgroupV2Unhelpful)

	api := &stubInspector{mounts: []container.MountPoint{
		{Type: mount.TypeVolume, Name: "boid_boid_state", Destination: volume},
	}}

	res, err := DetectDaemonStateVolumes(context.Background(), api, filepath.Join(link, "state"))
	if err != nil {
		t.Fatalf("DetectDaemonStateVolumes: %v", err)
	}
	if !res.StateVolumeExpected {
		t.Fatal("StateVolumeExpected = false for a data root reached through a symlinked ancestor: " +
			"silent, empty, and the state volume stays mountable")
	}
	if !res.DataHomeCovered {
		t.Error("DataHomeCovered = false; the resolved data root is inside the reserved volume")
	}
}

// TestDetectDaemonStateVolumes_unresolvableDataHomeWarns is the tie break for
// the resolution itself: if the data root cannot be resolved, the lexical
// comparison below is meaningless, so the answer is "could not tell", not "on
// the root mount". Same rule as every other undecidable in this file.
func TestDetectDaemonStateVolumes_unresolvableDataHomeWarns(t *testing.T) {
	root := realTempDir(t)
	installProcFixtures(t, mountInfoFixture(""), cgroupV2Unhelpful)

	res, err := DetectDaemonStateVolumes(context.Background(), &stubInspector{}, filepath.Join(root, "not-created-yet"))
	if err == nil {
		t.Fatal("want an error when the data root cannot be resolved: an unresolvable path compares against nothing but \"/\"")
	}
	if !res.StateVolumeExpected {
		t.Error("StateVolumeExpected = false after a failed resolution; a check that did not run must not present as 'nothing to protect'")
	}
}

// TestDetectDaemonStateVolumes_unresolvableDataHomeStillReportsItWhenIdentified
// is the other half of that tie break, and the regression test for [round 3
// Minor 1, codex review]: the resolution failure has to survive a run in which
// every SUBSEQUENT step succeeds.
//
// The test above reaches the warning through the returned error, but only
// because its fixture ALSO has no container id — so the resolution failure and
// the identification failure are folded together and either one alone would
// keep the test green. Here identification succeeds and the inspect succeeds,
// which is the shape the first implementation dropped on the floor: `undecided`
// was computed, used on the error paths, and then never looked at again, so a
// data root the daemon could not resolve produced a nil error, a populated
// Names, a DataHomeCovered computed against an unresolved path, and a startup
// log with nothing wrong in it.
//
// It cannot be reported as the returned error either — the caller reads that as
// "containment is INACTIVE" and the volumes ARE reserved here — so it rides on
// DataHomeUnresolved. The caller-side half (a WARN of its own, distinct from
// the INACTIVE one) is pinned by internal/server's
// TestReservedDaemonStateVolumes_unresolvedDataHomeWarnsWithoutClaimingInactive.
func TestDetectDaemonStateVolumes_unresolvableDataHomeStillReportsItWhenIdentified(t *testing.T) {
	root := realTempDir(t)
	volume := filepath.Join(root, "vol")
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A BROKEN symlink, not a missing directory: it is the shape an operator
	// actually produces (a data root pointed at a volume path that is no
	// longer mounted), and it fails EvalSymlinks while leaving every other
	// step — filepath.Abs, the mountinfo parse, the inspect — perfectly
	// healthy.
	dataHome := filepath.Join(volume, "state")
	if err := os.Symlink(filepath.Join(root, "gone"), dataHome); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	installProcFixtures(t, mountInfoFixture(podmanContainerID, volume), cgroupV2Unhelpful)

	api := &stubInspector{mounts: []container.MountPoint{
		{Type: mount.TypeVolume, Name: "boid_boid_state", Destination: volume},
	}}

	res, err := DetectDaemonStateVolumes(context.Background(), api, dataHome)
	if err != nil {
		t.Fatalf("DetectDaemonStateVolumes: %v; a data root that will not resolve must not read as 'containment INACTIVE' "+
			"when the container was identified and its volumes were named", err)
	}
	if len(res.Names) != 1 || res.Names[0] != "boid_boid_state" {
		t.Errorf("Names = %v, want [boid_boid_state]", res.Names)
	}
	if res.DataHomeUnresolved == nil {
		t.Fatal("DataHomeUnresolved = nil after the data root failed to resolve: the failure is dropped on the only path where " +
			"nothing else reports it, so the operator sees a completely clean startup log while data_home_covered was computed " +
			"against a path the daemon could not resolve")
	}
	if !errors.Is(res.DataHomeUnresolved, os.ErrNotExist) {
		t.Errorf("DataHomeUnresolved = %v, want the resolution's own failure (a dangling symlink is ENOENT)", res.DataHomeUnresolved)
	}
	if !strings.Contains(res.DataHomeUnresolved.Error(), dataHome) {
		t.Errorf("DataHomeUnresolved = %v, does not name the data root %s an operator would have to go fix", res.DataHomeUnresolved, dataHome)
	}
	// Computed against the absolute-but-unresolved spelling, deliberately —
	// see DataHomeUnresolved's own doc comment for why forcing it false would
	// be an equally unsupported claim in the other direction.
	if !res.DataHomeCovered {
		t.Error("DataHomeCovered = false; the unresolved data root still lexically sits inside the reserved volume, and a false " +
			"here would assert the crown jewels are outside every reserved volume on no evidence")
	}
}

// --- [round 2 Major 2] no error may reach a caller that reads it second -----

// TestDetectDaemonStateVolumes_everyErrorCarriesStateVolumeExpected is the
// invariant the CALLER's contract rests on: reservedDaemonStateVolumes decides
// between silence and a warning, and the only shape it may treat as silence is
// (StateVolumeExpected false, nil error). If any error path can also carry
// StateVolumeExpected false, a caller that consults the flag first drops the
// error unread and the operator is never told containment is off — which is
// how the docker-client construction failure shipped.
//
// So: every error return, enumerated, asserted to carry the flag. This is the
// table the "no silent path" claim in
// docs/plans/workspace-home-volume-persistence.md is checked against.
func TestDetectDaemonStateVolumes_everyErrorCarriesStateVolumeExpected(t *testing.T) {
	root := realTempDir(t)
	volume := filepath.Join(root, "vol")
	state := filepath.Join(volume, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	onOwnMount := mountInfoFixture(podmanContainerID, volume)
	noContainerID := mountInfoFixture("", volume)

	cases := []struct {
		name         string
		mountInfo    string
		cgroup       string
		unreadable   bool
		api          SelfContainerInspector
		dataHome     string
		shortTimeout bool
	}{
		{
			// Undecidable on BOTH axes. The two "cannot tell" reasons are
			// separate repairs, so both have to survive into the error — see
			// the errors.Join in DetectDaemonStateVolumes.
			name: "data root unknown and no container id", mountInfo: noContainerID, cgroup: cgroupV2Unhelpful,
			api: &stubInspector{}, dataHome: "",
		},
		{
			name: "data root unresolvable and no container id", mountInfo: noContainerID, cgroup: cgroupV2Unhelpful,
			api: &stubInspector{}, dataHome: filepath.Join(root, "gone"),
		},
		{
			name: "mountinfo unreadable", mountInfo: onOwnMount, cgroup: cgroupV2Unhelpful,
			unreadable: true, api: &stubInspector{}, dataHome: state,
		},
		{
			name: "container id not found", mountInfo: noContainerID, cgroup: cgroupV2Unhelpful,
			api: &stubInspector{}, dataHome: state,
		},
		{
			name: "container id ambiguous", mountInfo: noContainerID, cgroup: cgroupNestedDocker,
			api: &stubInspector{}, dataHome: state,
		},
		{
			// The shape [round 2 Major 2] is about: production could not build
			// a docker client, so detection is handed none.
			name: "no docker client", mountInfo: onOwnMount, cgroup: cgroupV2Unhelpful,
			api: nil, dataHome: state,
		},
		{
			name: "inspect failed", mountInfo: onOwnMount, cgroup: cgroupV2Unhelpful,
			api: &stubInspector{err: errors.New("engine unreachable")}, dataHome: state,
		},
		{
			name: "inspect timed out", mountInfo: onOwnMount, cgroup: cgroupV2Unhelpful,
			api: hangingInspector{}, dataHome: state, shortTimeout: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installProcFixtures(t, tc.mountInfo, tc.cgroup)
			if tc.unreadable {
				readSelfMountInfo = func() ([]byte, error) { return nil, errors.New("permission denied") }
			}
			if tc.shortTimeout {
				withSelfInspectTimeout(t, 200*time.Millisecond)
			}

			res, err := DetectDaemonStateVolumes(context.Background(), tc.api, tc.dataHome)
			if err == nil {
				t.Fatalf("want an error; got %+v", res)
			}
			if !res.StateVolumeExpected {
				t.Errorf("error %v returned with StateVolumeExpected=false: a caller that reads the flag first discards this error unread and stays silent", err)
			}
			if len(res.Names) != 0 {
				t.Errorf("Names = %v, want none alongside an error", res.Names)
			}
		})
	}
}

// mountInfoFixture renders a synthetic /proc/self/mountinfo whose only mount
// points are "/" and the given paths, optionally carrying containerID in the
// runtime-injected /etc/hostname line the way both engines do.
//
// Generated rather than hand-written because the tests that use it compare
// against REAL directories: a fixture naming a path that does not exist on
// disk cannot distinguish "the data root resolved and landed on this mount"
// from "the data root failed to resolve", which is the entire distinction
// under test. The field layout is mountinfo's own:
//
//	id parent major:minor root mountPoint options... - fstype source superOpts
func mountInfoFixture(containerID string, mountPoints ...string) string {
	s := rootMountLine + mountLines(mountPoints...)
	if containerID != "" {
		s += fmt.Sprintf("961 30 0:47 /containers/overlay-containers/%s/userdata/hostname /etc/hostname rw,nosuid,nodev,relatime - tmpfs tmpfs rw\n",
			containerID)
	}
	return s
}

const rootMountLine = "30 1 252:0 / / rw,relatime shared:1 - ext4 /dev/mapper/ubuntu--vg-ubuntu--lv rw\n"

// mountLines renders just the mount-point lines, so a test can append real
// directories to a VERBATIM fixture instead of replacing it. That combination
// is what keeps a fixture's own evidence intact (which runtime-injected paths
// it carries, and therefore whether a container id can be extracted from it)
// while still giving DetectDaemonStateVolumes a data root that exists on the
// machine running the test.
func mountLines(mountPoints ...string) string {
	var b strings.Builder
	for i, mp := range mountPoints {
		fmt.Fprintf(&b, "%d 30 252:0 /volumes/x/_data %s rw,relatime - ext4 /dev/mapper/ubuntu--vg-ubuntu--lv rw\n",
			100+i, escapeMountInfoField(mp))
	}
	return b.String()
}

// escapeMountInfoField is unescapeMountInfoField's inverse, applied to the
// generated fixtures so a temp directory containing a space (TMPDIR is not
// ours to choose) round-trips through the parser instead of silently splitting
// into two fields.
func escapeMountInfoField(s string) string {
	return strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace(s)
}

// realTempDir is t.TempDir() with every symlink resolved, so a test can put a
// path it created into a mountinfo fixture and have it compare equal to what
// the production resolution produces. On a host whose TMPDIR itself goes
// through a symlink (/tmp -> /private/tmp and friends) the unresolved form
// would not.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

// hangingInspector stands for the engine socket that accepts the connection
// and then goes silent — a shape no `err != nil` check covers, because there
// is no error until someone imposes a deadline. It answers only when the
// context it was given expires; the 3s escape hatch exists purely so that a
// build WITHOUT a deadline fails this test in seconds instead of hanging the
// whole package until go test's 10 minute panic.
type hangingInspector struct{}

func (hangingInspector) ContainerInspect(ctx context.Context, _ string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	select {
	case <-ctx.Done():
		return client.ContainerInspectResult{}, ctx.Err()
	case <-time.After(3 * time.Second):
		return client.ContainerInspectResult{}, nil
	}
}

// withSelfInspectTimeout shrinks the startup self-inspect deadline for the
// duration of one test. The production value is sized for a slow engine, not
// for a test suite.
func withSelfInspectTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := selfInspectTimeout
	t.Cleanup(func() { selfInspectTimeout = prev })
	selfInspectTimeout = d
}

// dockerAPI (the container backend's full engine interface) must satisfy the
// narrow inspector this feature needs, so production can pass the same client
// without a second connection or an adapter.
var _ SelfContainerInspector = (dockerAPI)(nil)
