// Command fakepodman is the Podman test double of the podman package's tests:
// it answers the few commands the runtimes issue against a directory of JSON
// files (containers, volumes and networks), records every invocation and honors
// a few switches tests flip (an outage, the exit codes of exec and probe
// commands). It touches no engine. It is a separate, plain binary because a test
// binary built with the race detector is too slow to start for every command.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

func main() {
	dir := os.Getenv("KODAMA_FAKE_PODMAN")
	if dir == "" {
		fmt.Fprintln(os.Stderr, "fakepodman: KODAMA_FAKE_PODMAN names no state directory")
		os.Exit(2)
	}
	os.Exit(fakePodman(dir, os.Args[1:]))
}

// createdContainer is what the double records of one `podman create`.
type createdContainer struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	ExitCode   int               `json:"exitCode"`
	Image      string            `json:"image"`
	Labels     map[string]string `json:"labels"`
	Volumes    []string          `json:"volumes"`
	Env        []string          `json:"env"`
	Tmpfs      []string          `json:"tmpfs"`
	Entrypoint string            `json:"entrypoint"`
	Args       []string          `json:"args"`
	User       string            `json:"user"`
	Networks   []string          `json:"networks"`
	Aliases    []string          `json:"aliases"`
	CapAdd     []string          `json:"capAdd"`
	Flags      []string          `json:"flags"`
	Argv       []string          `json:"argv"`
	Foreign    bool              `json:"foreign,omitempty"`
}

type fakeVolume struct {
	Name      string            `json:"Name"`
	Labels    map[string]string `json:"Labels"`
	CreatedAt string            `json:"CreatedAt"`
}

type fakeNetwork struct {
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Labels   map[string]string `json:"labels"`
	Internal bool              `json:"internal"`
}

// fakePodman answers the commands the runtimes issue against a directory of
// JSON files: containers, volumes and networks, every invocation, and a few
// switches tests flip (an outage, the exit codes of exec and probe commands).
func fakePodman(dir string, args []string) int {
	if len(args) == 0 {
		return 2
	}
	if f, err := os.OpenFile(filepath.Join(dir, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, args[0])
		f.Close()
	}
	if f, err := os.OpenFile(filepath.Join(dir, "argv.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		raw, _ := json.Marshal(args)
		fmt.Fprintln(f, string(raw))
		f.Close()
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "Error: "+format+"\n", a...)
		return code
	}
	if _, err := os.Stat(filepath.Join(dir, "down")); err == nil {
		return fail(125, "unable to connect to Podman socket: the machine is down")
	}
	readNum := func(name string, def int) int {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return def
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return def
		}
		return n
	}
	readStr := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(raw))
	}
	containers := filepath.Join(dir, "containers")
	volumes := filepath.Join(dir, "volumes")
	networks := filepath.Join(dir, "networks")
	for _, d := range []string{containers, volumes, networks} {
		_ = os.MkdirAll(d, 0o700)
	}
	loadAll := func() []createdContainer {
		entries, _ := os.ReadDir(containers)
		var out []createdContainer
		for _, e := range entries {
			raw, err := os.ReadFile(filepath.Join(containers, e.Name()))
			var c createdContainer
			if err == nil && json.Unmarshal(raw, &c) == nil {
				out = append(out, c)
			}
		}
		return out
	}
	load := func(ref string) (createdContainer, bool) {
		for _, c := range loadAll() {
			if c.ID == ref || c.Name == ref {
				return c, true
			}
		}
		return createdContainer{}, false
	}
	save := func(c createdContainer) {
		raw, _ := json.Marshal(c)
		_ = os.WriteFile(filepath.Join(containers, c.ID+".json"), raw, 0o600)
	}
	missing := func(kind string) int { return fail(1, "no such %s", kind) }
	usedVolume := func(name string) []string {
		var ids []string
		for _, c := range loadAll() {
			for _, v := range c.Volumes {
				if src, _, _ := strings.Cut(v, ":"); src == name {
					ids = append(ids, c.ID)
				}
			}
		}
		return ids
	}
	usedNetwork := func(name string) []string {
		var ids []string
		for _, c := range loadAll() {
			if slices.Contains(c.Networks, name) {
				ids = append(ids, c.ID)
			}
		}
		return ids
	}
	token := func() string {
		var b [8]byte
		_, _ = rand.Read(b[:])
		return hex.EncodeToString(b[:])
	}
	// object helpers for volume and network JSON files
	readJSON := func(path string, v any) bool {
		raw, err := os.ReadFile(path)
		return err == nil && json.Unmarshal(raw, v) == nil
	}
	writeJSON := func(path string, v any) {
		raw, _ := json.Marshal(v)
		_ = os.WriteFile(path, raw, 0o600)
	}
	labelArg := func(l map[string]string, kv string) {
		k, v, _ := strings.Cut(kv, "=")
		l[k] = v
	}
	// taken reports whether the name of a pending object is already in use.
	taken := func(kind string, data json.RawMessage) bool {
		switch kind {
		case "container":
			var c createdContainer
			if json.Unmarshal(data, &c) != nil {
				return true
			}
			_, ok := load(c.Name)
			return ok
		case "volume":
			var v fakeVolume
			return json.Unmarshal(data, &v) != nil || readJSON(filepath.Join(volumes, v.Name+".json"), &fakeVolume{})
		}
		var n fakeNetwork
		return json.Unmarshal(data, &n) != nil || readJSON(filepath.Join(networks, n.Name+".json"), &fakeNetwork{})
	}
	// pend records an object whose creation the engine completes later (__land).
	pend := func(kind, id string, v any) {
		_ = os.MkdirAll(filepath.Join(dir, "pending"), 0o700)
		raw, _ := json.Marshal(v)
		writeJSON(filepath.Join(dir, "pending", kind+"."+id+".json"), map[string]any{"kind": kind, "id": id, "data": json.RawMessage(raw)})
	}

	switch args[0] {
	case "create":
		c := createdContainer{Labels: map[string]string{}, Status: "created", Argv: args[1:]}
		rest := args[1:]
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			value := func() string { i++; return rest[i] }
			switch {
			case a == "--name":
				c.Name = value()
			case a == "--label":
				labelArg(c.Labels, value())
			case a == "--volume":
				c.Volumes = append(c.Volumes, value())
			case a == "--env":
				c.Env = append(c.Env, value())
			case a == "--tmpfs":
				c.Tmpfs = append(c.Tmpfs, value())
			case a == "--user":
				c.User = value()
			case a == "--network":
				c.Networks = append(c.Networks, value())
			case a == "--network-alias":
				c.Aliases = append(c.Aliases, value())
			case a == "--cap-add":
				c.CapAdd = append(c.CapAdd, value())
			case a == "--entrypoint":
				c.Entrypoint = value()
			case a == "--read-only":
				c.Flags = append(c.Flags, a)
			case strings.HasPrefix(a, "--") && strings.Contains(a, "="):
				c.Flags = append(c.Flags, a)
			case strings.HasPrefix(a, "--"):
				value() // a flag with its value (--cpus, --memory, ...)
			default:
				c.Image, c.Args = a, rest[i+1:]
				i = len(rest)
			}
		}
		if _, ok := load(c.Name); ok {
			return fail(125, "creating container storage: the container name %q is already in use", c.Name)
		}
		sum := sha256.Sum256([]byte(c.Name + token()))
		c.ID = hex.EncodeToString(sum[:])
		switch readStr("create-fault") {
		case "": // no fault
		case "container:lose": // the engine creates it, the answer is lost
			save(c)
			return fail(125, "connection reset by peer")
		case "container:late": // the answer is lost and the engine completes it later
			pend("container", c.ID, c)
			return fail(125, "context deadline exceeded")
		}
		save(c)
		fmt.Println(c.ID)
		return 0
	case "container":
		if len(args) < 3 {
			return 2
		}
		c, ok := load(args[len(args)-1])
		switch args[1] {
		case "exists":
			if ok {
				return 0
			}
			return 1
		case "inspect":
			if !ok {
				return missing("container")
			}
			cfg := map[string]any{"Labels": c.Labels}
			host := map[string]any{}
			if slices.Contains(c.Flags, "--userns=auto") && readStr("no-userns") == "" {
				cfg["Annotations"] = map[string]string{"io.podman.annotations.userns": "auto"}
				host["IDMappings"] = map[string]any{"UidMap": []string{"0:1:65533"}, "GidMap": []string{"0:1:65533"}}
			}
			out, _ := json.Marshal([]map[string]any{{
				"Id": c.ID, "Name": "/" + c.Name, "Config": cfg, "HostConfig": host,
				"State": map[string]any{"Status": c.Status, "Running": c.Status == "running", "ExitCode": c.ExitCode},
			}})
			fmt.Println(string(out))
			return 0
		}
		return 2
	case "start":
		if readStr("fail-start") != "" {
			return fail(125, "cannot start the container")
		}
		c, ok := load(args[len(args)-1])
		if !ok {
			return missing("container")
		}
		c.Status = "running"
		if c.Labels["kodama.optimi.com/resource-kind"] == "probe" {
			// A tcp probe runs to completion at once; its exit code is probe-code.
			c.Status, c.ExitCode = "exited", readNum("probe-code", 1)
			save(c)
			return c.ExitCode
		}
		if len(c.Args) > 0 && c.Args[0] == "prepare" {
			// A preparation runs to completion at once (exit code prepare-code).
			c.Status, c.ExitCode = "exited", readNum("prepare-code", 0)
		}
		save(c)
		return 0
	case "stop":
		if readStr("fail-stop") != "" {
			return fail(125, "cannot stop the container")
		}
		c, ok := load(args[len(args)-1])
		if !ok {
			return missing("container")
		}
		c.Status = "exited"
		save(c)
		return 0
	case "rm":
		c, ok := load(args[len(args)-1])
		if !ok {
			return missing("container")
		}
		if readStr("fail-rm-probe") != "" && c.Labels["kodama.optimi.com/resource-kind"] == "probe" {
			return fail(125, "cannot remove the container")
		}
		_ = os.Remove(filepath.Join(containers, c.ID+".json"))
		return 0
	case "__exit": // test control: __exit REF CODE
		c, ok := load(args[1])
		if !ok {
			return missing("container")
		}
		c.Status = "exited"
		c.ExitCode, _ = strconv.Atoi(args[2])
		save(c)
		return 0
	case "__plant": // test control: __plant NAME [LABEL=VALUE...] — a foreign container
		c := createdContainer{Name: args[1], Status: "running", Labels: map[string]string{}, Foreign: true}
		for _, kv := range args[2:] {
			labelArg(c.Labels, kv)
		}
		sum := sha256.Sum256([]byte("foreign" + c.Name + token()))
		c.ID = hex.EncodeToString(sum[:])
		save(c)
		fmt.Println(c.ID)
		return 0
	case "volume":
		if len(args) < 3 {
			return 2
		}
		name := args[len(args)-1]
		path := filepath.Join(volumes, name+".json")
		var v fakeVolume
		have := readJSON(path, &v)
		switch args[1] {
		case "exists":
			if have {
				return 0
			}
			return 1
		case "create":
			if have {
				return fail(125, "volume with name %s already exists: volume already exists", name)
			}
			v = fakeVolume{Name: name, Labels: map[string]string{}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano) + "-" + token()}
			for i := 2; i < len(args)-1; i++ {
				if args[i] == "--label" {
					i++
					labelArg(v.Labels, args[i])
				}
			}
			switch readStr("create-fault") {
			case "volume:lose":
				writeJSON(path, v)
				return fail(125, "connection reset by peer")
			case "volume:late":
				pend("volume", name, v)
				return fail(125, "context deadline exceeded")
			}
			writeJSON(path, v)
			fmt.Println(name)
			return 0
		case "inspect":
			if !have {
				return fail(125, "no such volume %s", name)
			}
			out, _ := json.Marshal([]fakeVolume{v})
			fmt.Println(string(out))
			return 0
		case "rm":
			if !have {
				return fail(1, "no volume with name %q found: no such volume", name)
			}
			if ids := usedVolume(name); len(ids) > 0 {
				return fail(2, "volume %s is being used by the following container(s): %s: volume is being used", name, strings.Join(ids, ", "))
			}
			_ = os.Remove(path)
			return 0
		}
		return 2
	case "network":
		if len(args) < 3 {
			return 2
		}
		name := args[len(args)-1]
		path := filepath.Join(networks, name+".json")
		var n fakeNetwork
		have := readJSON(path, &n)
		switch args[1] {
		case "exists":
			if have {
				return 0
			}
			return 1
		case "create":
			if have {
				return fail(125, "network name %s already used: network already exists", name)
			}
			sum := sha256.Sum256([]byte(name + token()))
			n = fakeNetwork{Name: name, ID: hex.EncodeToString(sum[:]), Labels: map[string]string{}}
			for i := 2; i < len(args)-1; i++ {
				switch args[i] {
				case "--label":
					i++
					labelArg(n.Labels, args[i])
				case "--internal":
					n.Internal = true
				}
			}
			switch readStr("create-fault") {
			case "network:lose":
				writeJSON(path, n)
				return fail(125, "connection reset by peer")
			case "network:late":
				pend("network", name, n)
				return fail(125, "context deadline exceeded")
			}
			writeJSON(path, n)
			fmt.Println(name)
			return 0
		case "inspect":
			if !have {
				return fail(125, "network %s: network not found", name)
			}
			out, _ := json.Marshal([]fakeNetwork{n})
			fmt.Println(string(out))
			return 0
		case "rm":
			if !have {
				return fail(1, "unable to find network with name or ID %s: network not found", name)
			}
			if ids := usedNetwork(name); len(ids) > 0 {
				return fail(2, "%q has associated containers with it. Use -f to forcibly delete containers and pods: network is being used", name)
			}
			_ = os.Remove(path)
			return 0
		}
		return 2
	case "ps":
		var vol string
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "volume="); ok {
				vol = v
			}
		}
		for _, c := range loadAll() {
			if vol == "" || slices.Contains(usedVolume(vol), c.ID) {
				fmt.Printf("%s %s\n", c.ID, c.Status)
			}
		}
		return 0
	case "exec":
		if _, ok := load(args[1]); !ok {
			return missing("container")
		}
		return readNum("exec-code", 1)
	case "run": // the tcp probe container
		p := createdContainer{Labels: map[string]string{}, Status: "exited", Argv: args[1:]}
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--name":
				i++
				p.Name = args[i]
			case "--label":
				i++
				labelArg(p.Labels, args[i])
			}
		}
		if _, ok := load(p.Name); ok && p.Name != "" {
			return fail(125, "creating container storage: the container name %q is already in use", p.Name)
		}
		if readStr("probe-linger") != "" && p.Name != "" {
			// The client was cancelled, the engine ran the container and keeps it.
			sum := sha256.Sum256([]byte(p.Name + token()))
			p.ID = hex.EncodeToString(sum[:])
			save(p)
		}
		return readNum("probe-code", 1)
	case "__land": // test control: the engine completes the creations whose answers were lost
		entries, _ := os.ReadDir(filepath.Join(dir, "pending"))
		for _, e := range entries {
			var w struct {
				Kind string          `json:"kind"`
				ID   string          `json:"id"`
				Data json.RawMessage `json:"data"`
			}
			path := filepath.Join(dir, "pending", e.Name())
			if !readJSON(path, &w) {
				continue
			}
			target := map[string]string{"container": containers, "volume": volumes, "network": networks}[w.Kind]
			_ = os.Remove(path)
			// The engine refuses the late request when the name is taken.
			if taken(w.Kind, w.Data) {
				_ = os.WriteFile(filepath.Join(dir, "refused.log"), []byte(w.Kind+" "+w.ID+"\n"), 0o600)
				continue
			}
			_ = os.WriteFile(filepath.Join(target, w.ID+".json"), w.Data, 0o600)
		}
		return 0
	case "logs":
		if _, ok := load(args[len(args)-1]); !ok {
			return missing("container")
		}
		fmt.Println("service log line")
		return 0
	}
	return 0
}
