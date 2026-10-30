package rmount

import (
	"strings"
	"testing"
)

func TestConfig(t *testing.T) {
	c := Config{WebDAVURL: "https://dav.real-debrid.com/", WebDAVUser: "alice", WebDAVPass: "s3cret",
		WebDAVPath: "/torrents/things/", MountPoint: "/data", UID: 99, GID: 100, ExtraArgs: []string{"--tpslimit", "4"}}
	if !c.Enabled() {
		t.Fatal("should be enabled")
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.Target(); got != "webdav:torrents/things" {
		t.Errorf("Target = %q", got)
	}
	if d := c.Describe(); strings.Contains(d, "s3cret") || !strings.Contains(d, "alice") {
		t.Errorf("Describe leaks or lacks info: %q", d)
	}
	args := strings.Join(c.Args(true), " ")
	for _, want := range []string{"mount webdav:torrents/things /data", "--read-only", "--allow-other", "--uid 99", "--gid 100", "--allow-non-empty", "--tpslimit 4"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	env := strings.Join(c.Env("OBSCURED"), "\n")
	for _, want := range []string{"RCLONE_CONFIG_WEBDAV_TYPE=webdav", "RCLONE_CONFIG_WEBDAV_URL=https://dav.real-debrid.com/", "RCLONE_CONFIG_WEBDAV_USER=alice", "RCLONE_CONFIG_WEBDAV_PASS=OBSCURED"} {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %q", want)
		}
	}
	if strings.Contains(env, "s3cret") {
		t.Error("plain password must not be passed through")
	}

	c.WebDAVPass = ""
	if err := c.Validate(); err == nil {
		t.Error("empty password should fail validation")
	}

	r := Config{Remote: "rd:torrents/things", MountPoint: "/data"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if r.Target() != "rd:torrents/things" || r.Env("x") != nil {
		t.Error("remote config should be used as-is")
	}
	if (Config{}).Enabled() {
		t.Error("empty config must be disabled")
	}
}

func TestMountedIn(t *testing.T) {
	info := `22 1 0:21 / / rw,relatime shared:1 - overlay overlay rw
130 22 0:50 / /data rw,nosuid,nodev,relatime shared:70 - fuse.rclone webdav:torrents/things rw,user_id=0,group_id=0,allow_other
131 22 0:51 / /mnt/my\040share rw - fuse.rclone x rw
`
	if !mountedIn(info, "/data") || !mountedIn(info, "/data/") {
		t.Error("/data should be mounted")
	}
	if !mountedIn(info, "/mnt/my share") {
		t.Error("escaped space not handled")
	}
	if mountedIn(info, "/index") || mountedIn(info, "/dat") {
		t.Error("false positive")
	}
}
