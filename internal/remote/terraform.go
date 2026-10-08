package remote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
)

func init() {
	snap.RegisterRecordKind("terraform", snap.RecordKind{Restore: restoreTF})
}

// tfSpec is a parsed terraform destroy, apply or state rm.
type tfSpec struct {
	Dir  string // where terraform runs, after -chdir
	Mode string // destroy, apply, state rm or state mv
}

func parseTF(args []string, cwd string) (tfSpec, bool) {
	sp := tfSpec{Dir: cwd}
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		a := args[i]
		if v, ok := strings.CutPrefix(a, "-chdir="); ok {
			sp.Dir = absIn(cwd, v)
			continue
		}
		return sp, false // -help, -version and friends
	}
	if i >= len(args) {
		return sp, false
	}
	sub, rest := args[i], args[i+1:]
	for _, a := range rest {
		switch strings.TrimLeft(a, "-") {
		case "help", "h":
			return sp, false
		}
	}
	switch sub {
	case "destroy":
		sp.Mode = "destroy"
	case "apply":
		sp.Mode = "apply"
		for _, a := range rest {
			if a == "-destroy" || a == "--destroy" || a == "-destroy=true" {
				sp.Mode = "destroy"
			}
		}
	case "state":
		if len(rest) > 0 && (rest[0] == "rm" || rest[0] == "mv") {
			for _, a := range rest[1:] {
				if a == "-dry-run" || a == "--dry-run" {
					return sp, false
				}
			}
			sp.Mode = "state " + rest[0]
		} else {
			return sp, false
		}
	default:
		return sp, false
	}
	return sp, true
}

func tfTargets(c call) []Target {
	sp, ok := parseTF(c.argv[1:], c.cwd)
	if !ok {
		return nil
	}
	t := newTool(c, []string{"-chdir=" + sp.Dir})
	t.Cwd = sp.Dir
	return []Target{{Tool: t.Name, Describe: "the " + t.Name + " state of " + ui.HomeRel(sp.Dir) + " (resources themselves and the data in them are not saved)", Capture: func(dir string) (*snap.Record, error) {
		return captureTF(dir, t, sp)
	}}}
}

// tfState is what a terraform record keeps in its metadata.
type tfState struct {
	Tool      tool   `json:"tool"`
	Mode      string `json:"mode"`
	Workspace string `json:"workspace"`
	Serial    int64  `json:"serial"`
	Lineage   string `json:"lineage"`
}

// stateFile is the part of a Terraform state file Oopsy reads.
type stateFile struct {
	Serial    int64  `json:"serial"`
	Lineage   string `json:"lineage"`
	Resources []struct {
		Module    string `json:"module"`
		Mode      string `json:"mode"`
		Type      string `json:"type"`
		Name      string `json:"name"`
		Instances []any  `json:"instances"`
	} `json:"resources"`
}

// addresses lists the managed resources in a state, as terraform names them.
func (s stateFile) addresses() []string {
	var out []string
	for _, r := range s.Resources {
		if r.Mode != "managed" || len(r.Instances) == 0 {
			continue
		}
		a := r.Type + "." + r.Name
		if r.Module != "" {
			a = r.Module + "." + a
		}
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// workspace is the workspace terraform will use in dir.
func tfWorkspace(t tool) string {
	if w := lookupEnv(t.environ(), "TF_WORKSPACE"); w != "" {
		return w
	}
	data := lookupEnv(t.environ(), "TF_DATA_DIR")
	if data == "" {
		data = ".terraform"
	}
	if b, err := os.ReadFile(filepath.Join(absIn(t.Cwd, data), "environment")); err == nil {
		if w := strings.TrimSpace(string(b)); w != "" {
			return w
		}
	}
	return "default"
}

var tfEnv = []string{"TF_IN_AUTOMATION=1", "TF_INPUT=0"}

func captureTF(dir string, t tool, sp tfSpec) (*snap.Record, error) {
	out, err := t.run(captureTimeout, nil, tfEnv, "state", "pull")
	if err != nil {
		return nil, fmt.Errorf("%s %s: could not read the state first: %v", t.Name, sp.Mode, err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil // no state yet, so nothing to lose
	}
	var sf stateFile
	if err := json.Unmarshal(out, &sf); err != nil {
		return nil, fmt.Errorf("%s %s: the state is not valid JSON: %v", t.Name, sp.Mode, err)
	}
	if err := writeFile(dir, "terraform.tfstate", out); err != nil {
		return nil, fmt.Errorf("%s %s: %v", t.Name, sp.Mode, err)
	}
	ws := tfWorkspace(t)
	st := tfState{Tool: t, Mode: sp.Mode, Workspace: ws, Serial: sf.Serial, Lineage: sf.Lineage}
	where := ui.HomeRel(sp.Dir)
	if ws != "default" {
		where += ", workspace " + ws
	}
	label := fmt.Sprintf("%s state before %s in %s (%s, serial %d)", t.Name, sp.Mode, where, plural(len(sf.addresses()), "resource"), sf.Serial)
	return &snap.Record{Kind: "terraform", Label: label, Meta: meta(st), Size: int64(len(out))}, nil
}

// restoreTF pushes the saved state back. Terraform refuses a push whose
// serial is not newer than the current state's, and the command being
// undone raised the serial, so the saved state is pushed with its serial
// bumped past the current one: same lineage, same content, newer serial.
// That is exactly the check -force skips, so -force is never used. When
// the lineage differs the state was replaced by an unrelated one, and
// pushing could clobber it, so Oopsy stops and prints the manual command.
func restoreTF(s *snap.Snapshot, r snap.Record) snap.Outcome {
	fail := func(msg string) snap.Outcome { return snap.Outcome{Path: r.Label, Status: "failed", Detail: msg} }
	var st tfState
	if err := unmeta(r.Meta, &st); err != nil {
		return fail("the saved details are unreadable")
	}
	saved := filepath.Join(s.RecordDir(r), "terraform.tfstate")
	raw, err := os.ReadFile(saved)
	if err != nil {
		return fail("the saved state is missing")
	}
	var old stateFile
	_ = json.Unmarshal(raw, &old)
	env := append(append([]string(nil), tfEnv...), "TF_WORKSPACE="+st.Workspace)
	curRaw, err := st.Tool.run(restoreTimeout, nil, env, "state", "pull")
	if err != nil {
		return fail("could not read the current state: " + err.Error())
	}
	var cur stateFile
	if len(bytes.TrimSpace(curRaw)) > 0 {
		if err := json.Unmarshal(curRaw, &cur); err != nil {
			return fail("the current state is not valid JSON")
		}
		if cur.Lineage != "" && cur.Lineage != st.Lineage {
			return fail(fmt.Sprintf("the state was replaced by a different one since (lineage %s, saved %s), so Oopsy will not overwrite it. To push the saved state anyway: %s -chdir=%s state push -force %s",
				cur.Lineage, st.Lineage, st.Tool.Name, st.Tool.Cwd, saved))
		}
	}
	serial := old.Serial
	if cur.Serial >= serial {
		serial = cur.Serial + 1
	}
	push, err := withSerial(raw, serial)
	if err != nil {
		return fail("could not prepare the state: " + err.Error())
	}
	if _, err := st.Tool.run(restoreTimeout, bytes.NewReader(push), env, "state", "push", "-lock-timeout=10s", "-"); err != nil {
		return fail("terraform state push failed: " + err.Error())
	}
	return snap.Outcome{Path: r.Label, Status: "record", Detail: tfDetail(st, old, cur, serial)}
}

func tfDetail(st tfState, old, cur stateFile, serial int64) string {
	had := map[string]bool{}
	for _, a := range cur.addresses() {
		had[a] = true
	}
	was := map[string]bool{}
	for _, a := range old.addresses() {
		was[a] = true
	}
	var gone, untracked []string
	for _, a := range old.addresses() {
		if !had[a] {
			gone = append(gone, a)
		}
	}
	for _, a := range cur.addresses() {
		if !was[a] {
			untracked = append(untracked, a)
		}
	}
	msg := fmt.Sprintf("put back the saved state (%s) as serial %d", plural(len(old.addresses()), "resource"), serial)
	switch st.Mode {
	case "destroy":
		msg += ". Only the state is back: the destroyed resources are still gone, so run terraform plan to see them and terraform apply to create them again"
		if len(gone) > 0 {
			msg += " (" + listSome(gone, 4) + ")"
		}
		msg += ". Data inside them, such as database rows, bucket objects and disk contents, was not saved and will not come back"
	case "apply":
		msg += ". Changes the apply made to real resources are not undone: run terraform plan to see them and terraform apply to revert them"
		if len(untracked) > 0 {
			msg += ". Resources the apply created still exist but are no longer tracked, so remove them by hand or import them: " + listSome(untracked, 4)
		}
		msg += ". Data inside resources was not saved"
	default:
		msg += ", so terraform tracks the resources " + st.Mode + " changed again"
	}
	return msg
}

// withSerial returns the state with its serial set, keeping every other
// field byte for byte.
func withSerial(raw []byte, serial int64) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["serial"] = json.RawMessage(fmt.Sprint(serial))
	return json.MarshalIndent(m, "", "  ")
}
