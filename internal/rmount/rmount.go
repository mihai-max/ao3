// Package rmount runs `rclone mount` as a supervised child process so the
// container can read the archive zips straight from a WebDAV server such as
// Real-Debrid instead of a local folder.
package rmount

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config describes what to mount. Either Remote (an rclone "remote:path"
// from an rclone.conf) or the WebDAV fields must be set.
type Config struct {
	Remote     string // e.g. "rd:torrents/things"; wins over the WebDAV fields
	WebDAVURL  string // e.g. https://dav.real-debrid.com/
	WebDAVUser string
	WebDAVPass string
	WebDAVPath string // path inside the WebDAV share, e.g. torrents/things
	MountPoint string
	ExtraArgs  []string // appended to rclone mount
	UID, GID   int      // owner shown for mounted files; 0 keeps rclone's default
	Rclone     string   // rclone binary, default "rclone"
	Logger     *log.Logger
}

// FromEnv reads the AO3_WEBDAV_* / AO3_RCLONE_* variables.
func FromEnv(mountPoint string) Config {
	c := Config{
		Remote:     os.Getenv("AO3_RCLONE_REMOTE"),
		WebDAVURL:  envDefault("AO3_WEBDAV_URL", "https://dav.real-debrid.com/"),
		WebDAVUser: os.Getenv("AO3_WEBDAV_USER"),
		WebDAVPass: os.Getenv("AO3_WEBDAV_PASS"),
		WebDAVPath: envDefault("AO3_WEBDAV_PATH", "torrents/things"),
		MountPoint: mountPoint,
		ExtraArgs:  strings.Fields(os.Getenv("AO3_RCLONE_ARGS")),
	}
	c.UID, _ = strconv.Atoi(os.Getenv("PUID"))
	c.GID, _ = strconv.Atoi(os.Getenv("PGID"))
	return c
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Enabled reports whether a mount was configured at all.
func (c Config) Enabled() bool { return c.Remote != "" || c.WebDAVUser != "" }

// Target is the rclone remote spec that gets mounted.
func (c Config) Target() string {
	if c.Remote != "" {
		return c.Remote
	}
	return "webdav:" + strings.Trim(c.WebDAVPath, "/")
}

// Describe is a one-line, secret-free description for logs and the UI.
func (c Config) Describe() string {
	if c.Remote != "" {
		return fmt.Sprintf("rclone remote %s", c.Remote)
	}
	return fmt.Sprintf("WebDAV %s path /%s as %s", c.WebDAVURL, strings.Trim(c.WebDAVPath, "/"), c.WebDAVUser)
}

// Validate checks the configuration without touching the network.
func (c Config) Validate() error {
	if !c.Enabled() {
		return errors.New("no remote configured")
	}
	if c.Remote == "" {
		if c.WebDAVPass == "" {
			return errors.New("AO3_WEBDAV_PASS is empty: set it to your WebDAV password (for Real-Debrid, the WebDAV password on the account page, not the login password)")
		}
		if !strings.HasPrefix(c.WebDAVURL, "http://") && !strings.HasPrefix(c.WebDAVURL, "https://") {
			return fmt.Errorf("AO3_WEBDAV_URL %q must start with http:// or https://", c.WebDAVURL)
		}
	}
	if c.MountPoint == "" {
		return errors.New("mount point is empty")
	}
	return nil
}

// Args returns the rclone command line (without the binary).
func (c Config) Args(allowNonEmpty bool) []string {
	args := []string{"mount", c.Target(), c.MountPoint,
		"--read-only", "--allow-other",
		"--vfs-cache-mode", "off",
		"--vfs-read-chunk-size", "16M", "--vfs-read-chunk-size-limit", "off",
		"--buffer-size", "32M",
		"--dir-cache-time", "12h", "--attr-timeout", "1h", "--poll-interval", "0",
		"--no-modtime", "--no-checksum",
		"--contimeout", "30s", "--timeout", "2m", "--low-level-retries", "20", "--retries", "5",
		"--umask", "022", "--log-level", "NOTICE", "--stats", "0",
	}
	if c.UID > 0 {
		args = append(args, "--uid", strconv.Itoa(c.UID))
	}
	if c.GID > 0 {
		args = append(args, "--gid", strconv.Itoa(c.GID))
	}
	if allowNonEmpty {
		args = append(args, "--allow-non-empty")
	}
	return append(args, c.ExtraArgs...)
}

// Env returns the extra environment that defines the "webdav" remote when
// the WebDAV fields are used. obscuredPass must come from `rclone obscure`.
func (c Config) Env(obscuredPass string) []string {
	if c.Remote != "" {
		return nil
	}
	return []string{
		"RCLONE_CONFIG_WEBDAV_TYPE=webdav",
		"RCLONE_CONFIG_WEBDAV_URL=" + c.WebDAVURL,
		"RCLONE_CONFIG_WEBDAV_VENDOR=other",
		"RCLONE_CONFIG_WEBDAV_USER=" + c.WebDAVUser,
		"RCLONE_CONFIG_WEBDAV_PASS=" + obscuredPass,
	}
}

// Mount is a running, supervised rclone mount.
type Mount struct {
	cfg    Config
	env    []string
	logger *log.Logger
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	mounted  bool
	restarts int
	lastErr  string
	recent   []string
}

// State is a snapshot for the status page.
type State struct {
	Describe string
	Mounted  bool
	Restarts int
	LastErr  string
	Recent   []string
}

const (
	mountTimeout = 90 * time.Second
	maxRestarts  = 10
)

// Start launches rclone and returns once the mount point is live.
func Start(ctx context.Context, cfg Config) (*Mount, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Rclone == "" {
		cfg.Rclone = "rclone"
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if _, err := exec.LookPath(cfg.Rclone); err != nil {
		return nil, fmt.Errorf("rclone is not installed in this image: %w", err)
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		return nil, errors.New("/dev/fuse is missing: run the container with --device /dev/fuse --cap-add SYS_ADMIN (Unraid: Extra Parameters)")
	}
	if err := os.MkdirAll(cfg.MountPoint, 0o755); err != nil {
		return nil, err
	}
	// The main process later drops to PUID:PGID, and a non-root process cannot
	// signal or unmount what root mounted. So rclone runs as that user from
	// the start; the setuid fusermount3 in the image does the actual mount.
	if os.Getuid() == 0 && cfg.UID > 0 {
		os.Chmod("/dev/fuse", 0o666)
		os.Chown(cfg.MountPoint, cfg.UID, cfg.GID)
		ensureAccount(cfg.UID, cfg.GID)
	}
	if mounted(cfg.MountPoint) {
		cfg.Logger.Printf("rclone: %s is already a mount point, unmounting the stale mount", cfg.MountPoint)
		unmount(cfg.MountPoint)
	}
	allowNonEmpty := false
	if entries, _ := os.ReadDir(cfg.MountPoint); len(entries) > 0 {
		cfg.Logger.Printf("rclone: warning: %s is not empty, mounting over it hides %d existing entries", cfg.MountPoint, len(entries))
		allowNonEmpty = true
	}

	m := &Mount{cfg: cfg, logger: cfg.Logger, done: make(chan struct{})}
	if cfg.Remote == "" {
		obscured, err := obscure(ctx, cfg.Rclone, cfg.WebDAVPass)
		if err != nil {
			return nil, err
		}
		m.env = cfg.Env(obscured)
	}
	if os.Getenv("RCLONE_CONFIG") == "" {
		if _, err := os.Stat("/config/rclone.conf"); err == nil {
			m.env = append(m.env, "RCLONE_CONFIG=/config/rclone.conf")
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	ready := make(chan error, 1)
	go m.supervise(runCtx, allowNonEmpty, ready)
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			<-m.done
			return nil, err
		}
	case <-ctx.Done():
		cancel()
		<-m.done
		return nil, ctx.Err()
	}
	return m, nil
}

func obscure(ctx context.Context, rclone, pass string) (string, error) {
	cmd := exec.CommandContext(ctx, rclone, "obscure", "-")
	cmd.Stdin = strings.NewReader(pass)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("rclone obscure: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// supervise runs rclone, restarting it with backoff when it dies. The first
// successful mount (or the first failure) is reported on ready.
func (m *Mount) supervise(ctx context.Context, allowNonEmpty bool, ready chan<- error) {
	defer close(m.done)
	first := true
	backoff := 2 * time.Second
	for {
		err := m.runOnce(ctx, allowNonEmpty, func() {
			if first {
				first = false
				ready <- nil
			}
			backoff = 2 * time.Second
		})
		m.setMounted(false)
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		m.restarts++
		m.lastErr = err.Error()
		restarts := m.restarts
		m.mu.Unlock()
		if first {
			ready <- err
			return
		}
		m.logger.Printf("rclone: mount ended: %v (restart %d/%d in %s)", err, restarts, maxRestarts, backoff)
		if restarts > maxRestarts {
			m.logger.Printf("rclone: giving up after %d restarts", maxRestarts)
			return
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (m *Mount) runOnce(ctx context.Context, allowNonEmpty bool, onMounted func()) error {
	cmd := exec.Command(m.cfg.Rclone, m.cfg.Args(allowNonEmpty)...)
	cmd.Env = append(os.Environ(), m.env...)
	if os.Getuid() == 0 && m.cfg.UID > 0 {
		gid := m.cfg.GID
		if gid <= 0 {
			gid = m.cfg.UID
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
			Uid: uint32(m.cfg.UID), Gid: uint32(gid), Groups: []uint32{}}}
		// rclone wants a writable home for its (unused) config and cache dirs.
		cmd.Env = append(cmd.Env, "HOME=/tmp", "XDG_CONFIG_HOME=/tmp/.config", "XDG_CACHE_HOME=/tmp/.cache")
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = cmd.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting rclone: %w", err)
	}
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			m.logger.Printf("rclone: %s", line)
			m.remember(line)
		}
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Wait for the mount to show up.
	deadline := time.After(mountTimeout)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		select {
		case <-tick.C:
			if mounted(m.cfg.MountPoint) {
				break wait
			}
		case err := <-exited:
			<-logDone
			return fmt.Errorf("rclone exited before mounting: %v; last output: %s", err, m.lastLine())
		case <-deadline:
			cmd.Process.Kill()
			<-exited
			<-logDone
			return fmt.Errorf("mount did not appear within %s; last output: %s", mountTimeout, m.lastLine())
		case <-ctx.Done():
			m.stop(cmd, exited)
			<-logDone
			return ctx.Err()
		}
	}
	m.setMounted(true)
	m.logger.Printf("rclone: %s mounted at %s", m.cfg.Describe(), m.cfg.MountPoint)
	onMounted()

	select {
	case err := <-exited:
		<-logDone
		if err == nil {
			err = errors.New("rclone exited")
		}
		return err
	case <-ctx.Done():
		m.stop(cmd, exited)
		<-logDone
		return ctx.Err()
	}
}

// stop asks rclone to unmount and exit, then forces it if needed.
func (m *Mount) stop(cmd *exec.Cmd, exited <-chan error) {
	start := time.Now()
	m.logger.Printf("rclone: stopping (pid %d)", cmd.Process.Pid)
	cmd.Process.Signal(os.Interrupt)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		m.logger.Printf("rclone: still running after SIGINT, unmounting %s directly", m.cfg.MountPoint)
		unmount(m.cfg.MountPoint)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			m.logger.Printf("rclone: killing")
			cmd.Process.Kill()
			<-exited
		}
	}
	if !m.waitUnmounted(2 * time.Second) {
		unmount(m.cfg.MountPoint)
	}
	m.logger.Printf("rclone: stopped in %s, mounted=%v", time.Since(start).Round(time.Millisecond), mounted(m.cfg.MountPoint))
}

// Close unmounts and stops rclone.
func (m *Mount) Close() {
	m.cancel()
	<-m.done
}

// Done is closed when the supervisor has given up or been stopped.
func (m *Mount) Done() <-chan struct{} { return m.done }

// State returns a snapshot for the UI.
func (m *Mount) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return State{Describe: m.cfg.Describe(), Mounted: m.mounted, Restarts: m.restarts, LastErr: m.lastErr,
		Recent: append([]string(nil), m.recent...)}
}

// Mounted reports whether the mount point is currently live.
func (m *Mount) Mounted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounted
}

func (m *Mount) setMounted(v bool) {
	m.mu.Lock()
	m.mounted = v
	m.mu.Unlock()
}

func (m *Mount) remember(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recent = append(m.recent, line)
	if len(m.recent) > 20 {
		m.recent = m.recent[len(m.recent)-20:]
	}
}

func (m *Mount) lastLine() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.recent) == 0 {
		return "(none)"
	}
	return m.recent[len(m.recent)-1]
}

// ensureAccount gives uid/gid passwd and group entries if they have none,
// because fusermount3 refuses to work for a uid it cannot name.
func ensureAccount(uid, gid int) {
	if gid <= 0 {
		gid = uid
	}
	if _, err := user.LookupGroupId(strconv.Itoa(gid)); err != nil {
		appendLine("/etc/group", fmt.Sprintf("ao3:x:%d:\n", gid))
	}
	if _, err := user.LookupId(strconv.Itoa(uid)); err != nil {
		appendLine("/etc/passwd", fmt.Sprintf("ao3:x:%d:%d:ao3:/tmp:/sbin/nologin\n", uid, gid))
	}
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line)
}

func unmount(mountPoint string) {
	for _, tool := range []string{"fusermount3", "fusermount"} {
		if _, err := exec.LookPath(tool); err == nil {
			exec.Command(tool, "-uz", mountPoint).Run()
			return
		}
	}
	exec.Command("umount", "-l", mountPoint).Run()
}

// Unmounted blocks until the mount point is gone or the timeout passes.
func (m *Mount) waitUnmounted(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !mounted(m.cfg.MountPoint) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// mounted reports whether path is a mount point according to the kernel.
func mounted(path string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	return mountedIn(string(data), path)
}

// mountedIn parses mountinfo text. Field 5 is the mount point, with spaces
// escaped as \040.
func mountedIn(mountinfo, path string) bool {
	want := filepath.Clean(path)
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mp := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[4])
		if filepath.Clean(mp) == want {
			return true
		}
	}
	return false
}
