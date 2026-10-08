package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

func init() {
	snap.RegisterRecordKind("docker-volume", snap.RecordKind{Restore: restoreVolume})
}

// helperImage runs tar inside the volume. It is small and pinned, so the
// archive format never changes under a snapshot.
const helperImage = "alpine:3.20"

const volumeTimeout = 10 * time.Minute

var dockerGlobalValue = map[string]bool{"--context": true, "-c": true, "--host": true, "-H": true, "--config": true,
	"--log-level": true, "-l": true, "--tlscacert": true, "--tlscert": true, "--tlskey": true}

// dockerSpec is a parsed docker volume rm or prune.
type dockerSpec struct {
	Globals []string
	Prune   bool
	All     bool     // prune --all: named volumes too
	Filters []string // prune --filter values
	Names   []string // rm operands
}

func parseDocker(args []string) (dockerSpec, bool) {
	var d dockerSpec
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		name, _, hasVal := strings.Cut(args[i], "=")
		d.Globals = append(d.Globals, args[i])
		if dockerGlobalValue[name] && !hasVal && i+1 < len(args) {
			i++
			d.Globals = append(d.Globals, args[i])
		}
	}
	if i+1 >= len(args) || args[i] != "volume" {
		return d, false
	}
	sub, rest := args[i+1], args[i+2:]
	switch sub {
	case "rm", "remove":
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				d.Names = append(d.Names, a)
			}
		}
		return d, len(d.Names) > 0
	case "prune":
		d.Prune = true
		for j := 0; j < len(rest); j++ {
			a := rest[j]
			switch {
			case a == "-a" || a == "--all":
				d.All = true
			case a == "--filter" && j+1 < len(rest):
				j++
				d.Filters = append(d.Filters, rest[j])
			case strings.HasPrefix(a, "--filter="):
				d.Filters = append(d.Filters, strings.TrimPrefix(a, "--filter="))
			}
		}
		return d, true
	}
	return d, false
}

func dockerTargets(c call) []Target {
	d, ok := parseDocker(c.argv[1:])
	if !ok {
		return nil
	}
	t := newTool(c, d.Globals)
	if d.Prune {
		return []Target{{Tool: t.Name, Describe: "the contents of every volume the prune removes, as compressed tar archives", Capture: func(dir string) (*snap.Record, error) {
			return capturePrune(dir, t, d)
		}}}
	}
	var out []Target
	for _, name := range d.Names {
		out = append(out, Target{Tool: t.Name, Describe: "the contents of " + t.Name + " volume " + name + " as a compressed tar archive", Capture: func(dir string) (*snap.Record, error) {
			return captureVolumes(dir, t, []string{name})
		}})
	}
	return out
}

// volumeInfo is docker volume inspect output.
type volumeInfo struct {
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	Labels  map[string]string `json:"Labels"`
	Options map[string]string `json:"Options"`
	Archive string            `json:"archive"` // file inside the record folder
	Bytes   int64             `json:"bytes"`   // uncompressed tar size
}

type volumeState struct {
	Tool    tool         `json:"tool"`
	Volumes []volumeInfo `json:"volumes"`
}

// capturePrune finds the volumes prune would remove: unused ones, and
// without --all only anonymous ones, as Docker 23 and later do.
func capturePrune(dir string, t tool, d dockerSpec) (*snap.Record, error) {
	args := []string{"volume", "ls", "--quiet", "--filter", "dangling=true"}
	for _, f := range d.Filters {
		args = append(args, "--filter", f)
	}
	out, err := t.run(captureTimeout, nil, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("%s volume prune: could not list volumes first: %v", t.Name, err)
	}
	names := strings.Fields(string(out))
	if !d.All && len(names) > 0 {
		var anon []string
		for _, v := range inspectVolumes(t, names) {
			if _, ok := v.Labels["com.docker.volume.anonymous"]; ok {
				anon = append(anon, v.Name)
			}
		}
		names = anon
	}
	if len(names) == 0 {
		return nil, nil
	}
	return captureVolumes(dir, t, names)
}

func inspectVolumes(t tool, names []string) []volumeInfo {
	var vols []volumeInfo
	for _, n := range names {
		out, err := t.run(captureTimeout, nil, nil, "volume", "inspect", n)
		var v []volumeInfo
		if err == nil && json.Unmarshal(out, &v) == nil && len(v) == 1 {
			vols = append(vols, v[0])
		}
	}
	return vols
}

func captureVolumes(dir string, t tool, names []string) (*snap.Record, error) {
	vols := inspectVolumes(t, names)
	if len(vols) == 0 {
		return nil, nil // no such volume, nothing to lose
	}
	st := volumeState{Tool: t}
	var total int64
	for i, v := range vols {
		out, err := t.run(captureTimeout, nil, nil, "ps", "--all", "--quiet", "--filter", "volume="+v.Name)
		if err == nil && strings.TrimSpace(string(out)) != "" {
			if len(vols) == 1 {
				return nil, fmt.Errorf("%s volume %s is used by a container, so docker will refuse to remove it; nothing saved", t.Name, v.Name)
			}
			continue
		}
		v.Archive = fmt.Sprintf("volume%d.tar.zst", i)
		n, err := tarVolume(t, v.Name, filepath.Join(dir, v.Archive))
		if err != nil {
			return nil, fmt.Errorf("%s volume %s: not saved: %v", t.Name, v.Name, err)
		}
		v.Bytes = n
		total += n
		st.Volumes = append(st.Volumes, v)
	}
	if len(st.Volumes) == 0 {
		return nil, nil
	}
	label := fmt.Sprintf("%s volume %s (%s of files)", t.Name, st.Volumes[0].Name, human(total))
	if len(st.Volumes) > 1 {
		ns := make([]string, len(st.Volumes))
		for i, v := range st.Volumes {
			ns[i] = v.Name
		}
		label = fmt.Sprintf("%s %s (%s, %s of files)", t.Name, plural(len(ns), "volume"), listSome(ns, 3), human(total))
	}
	return &snap.Record{Kind: "docker-volume", Label: label, Meta: meta(st)}, nil
}

// spaceWriter fails once writing more would break the free space floor.
// It checks every 64 MB, so a large volume stops early, not at the end.
type spaceWriter struct {
	w       io.Writer
	dir     string
	n, next int64
}

func (s *spaceWriter) Write(p []byte) (int, error) {
	if s.n+int64(len(p)) > s.next {
		if err := checkSpace(s.dir, 64<<20); err != nil {
			return 0, err
		}
		s.next = s.n + 64<<20
	}
	n, err := s.w.Write(p)
	s.n += int64(n)
	return n, err
}

// tarVolume streams the volume's files as tar from a throwaway container
// into a zstd file. Streaming through stdout works with remote Docker
// hosts and Docker Desktop alike, where bind mounting a host folder may not.
func tarVolume(t tool, name, dst string) (int64, error) {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sw := &spaceWriter{w: f, dir: filepath.Dir(dst)}
	zw, err := zstd.NewWriter(sw, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return 0, err
	}
	counter := &countWriter{w: zw}
	_, err = t.runTo(volumeTimeout, nil, counter, nil, "run", "--rm", "--network", "none", "--pull", "missing",
		"--volume", name+":/v:ro", "--entrypoint", "tar", helperImage, "-C", "/v", "-cf", "-", ".")
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	return counter.n, f.Sync()
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// restoreVolume recreates each volume with its driver, options and labels,
// then unpacks the saved files into it. A volume that exists again is never
// overwritten: the saved files go into a new volume next to it.
func restoreVolume(s *snap.Snapshot, r snap.Record) snap.Outcome {
	fail := func(msg string) snap.Outcome { return snap.Outcome{Path: r.Label, Status: "failed", Detail: msg} }
	var st volumeState
	if err := unmeta(r.Meta, &st); err != nil {
		return fail("the saved details are unreadable")
	}
	t := st.Tool
	var done []string
	for _, v := range st.Volumes {
		target := v.Name
		if _, err := t.run(captureTimeout, nil, nil, "volume", "inspect", v.Name); err == nil {
			target = fmt.Sprintf("%s-oops-%d", v.Name, s.ID)
		}
		args := []string{"volume", "create"}
		if v.Driver != "" {
			args = append(args, "--driver", v.Driver)
		}
		for _, k := range sortedKeys(v.Labels) {
			args = append(args, "--label", k+"="+v.Labels[k])
		}
		for _, k := range sortedKeys(v.Options) {
			args = append(args, "--opt", k+"="+v.Options[k])
		}
		if _, err := t.run(restoreTimeout, nil, nil, append(args, target)...); err != nil {
			return fail(fmt.Sprintf("could not create volume %s: %v", target, err))
		}
		if err := untarVolume(t, filepath.Join(s.RecordDir(r), v.Archive), target); err != nil {
			return fail(fmt.Sprintf("created volume %s but could not unpack its files: %v", target, err))
		}
		if target != v.Name {
			done = append(done, fmt.Sprintf("%s exists again, so its saved files went into a new volume %s", v.Name, target))
		} else {
			done = append(done, fmt.Sprintf("recreated volume %s with its files (%s)", v.Name, human(v.Bytes)))
		}
	}
	return snap.Outcome{Path: r.Label, Status: "record", Detail: strings.Join(done, "; ")}
}

func untarVolume(t tool, archive, volume string) error {
	f, err := os.Open(archive)
	if err != nil {
		return errors.New("the saved archive is missing")
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()
	_, err = t.run(volumeTimeout, zr, nil, "run", "--rm", "-i", "--network", "none", "--pull", "missing",
		"--volume", volume+":/v", "--entrypoint", "tar", helperImage, "-C", "/v", "-xpf", "-")
	return err
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
