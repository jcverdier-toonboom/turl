// Command turl is a keyboard-driven HTTP/WebSocket client for the terminal.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"github.com/jcverdier/turl/internal/curl"
	"github.com/jcverdier/turl/internal/jq"
	"github.com/jcverdier/turl/internal/runner"
	"github.com/jcverdier/turl/internal/secrets"
	"github.com/jcverdier/turl/internal/spec"
	"github.com/jcverdier/turl/internal/tui"
	"github.com/jcverdier/turl/internal/vars"
)

const usage = `turl — HTTP & WebSocket client for the terminal

Usage:
  turl [--dir DIR]                       open the TUI
  turl init [DIR]                        create a workspace (default ./.turl) with examples
  turl run NAME [-e ENV] [-v k=v]... [-q] [--jq FILTER]
                                         run a request or flow, print the last body to stdout
  turl ls                                list flows and requests, grouped by collection
  turl collection ls|new NAME|rename OLD NEW|rm NAME
                                         manage collections (folders of requests/)
  turl add [-c COLLECTION] [-n NAME] [-f] CURL...
                                         register a request from a curl command, given as
                                         one quoted string, as raw arguments, or on stdin
  turl import NAME < curl.txt            create a request from a curl command
  turl curl NAME [-e ENV]                print a request as a curl command
  turl vars [-e ENV]                     print resolved variables
  turl secret ls|get NAME|refresh [NAME]...|clear
                                         secrets fetched by commands in secrets.yaml
  turl version

Workspace lookup: --dir, $TURL_DIR, nearest .turl/ upwards, ~/.config/turl
`

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "turl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	global := flag.NewFlagSet("turl", flag.ContinueOnError)
	dir := global.String("dir", "", "workspace directory")
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	rest := global.Args()
	cmd := ""
	if len(rest) > 0 {
		cmd, rest = rest[0], rest[1:]
	}

	if cmd == "init" {
		d := ".turl"
		if len(rest) > 0 {
			d = rest[0]
		}
		ws, err := spec.Init(d)
		if err != nil {
			return err
		}
		fmt.Println("initialized", ws.Dir)
		return nil
	}
	if cmd == "help" || cmd == "-h" {
		fmt.Print(usage)
		return nil
	}
	if cmd == "version" {
		fmt.Println("turl", version)
		return nil
	}

	ws, err := spec.Find(*dir)
	if err != nil {
		return err
	}
	store := secrets.Open(ws.Dir)
	vars.Secrets = store

	switch cmd {
	case "":
		if ws.IsEmpty() {
			if _, err := spec.Init(ws.Dir); err != nil {
				return err
			}
		}
		p := tea.NewProgram(tui.New(ws, store), tea.WithAltScreen())
		_, err := p.Run()
		return err
	case "ls", "list":
		return listCLI(ws)
	case "collection", "collections", "col":
		return collectionCLI(ws, rest)
	case "add":
		return addCLI(ws, rest)
	case "secret", "secrets":
		return secretCLI(store, rest)
	case "import":
		if len(rest) != 1 {
			return errors.New("usage: turl import NAME < curl.txt")
		}
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		req, err := curl.Import(string(b))
		if err != nil {
			return err
		}
		if err := spec.ValidName(rest[0]); err != nil {
			return err
		}
		if _, err := os.Stat(ws.RequestPath(rest[0])); err == nil {
			return fmt.Errorf("%s already exists", rest[0])
		}
		req.Name = rest[0]
		if err := ws.SaveRequest(req); err != nil {
			return err
		}
		fmt.Println("created", ws.RequestPath(rest[0]))
		return nil
	case "curl":
		fs := flag.NewFlagSet("curl", flag.ContinueOnError)
		env := fs.String("e", "", "environment")
		var sets multiFlag
		fs.Var(&sets, "v", "variable k=v (repeatable)")
		name, err := parseWithName(fs, rest)
		if err != nil {
			return err
		}
		it, err := ws.Lookup(name)
		if err != nil {
			return err
		}
		if it.Request == nil {
			return errors.New("curl export works on requests, not flows")
		}
		scope, _, err := cliScope(ws, *env, sets)
		if err != nil {
			return err
		}
		p, perr := runner.PrepareIn(ws, it.Request, scope)
		fmt.Println(curl.Export(p))
		if perr != nil {
			fmt.Fprintln(os.Stderr, "warning:", perr)
		}
		return nil
	case "vars":
		fs := flag.NewFlagSet("vars", flag.ContinueOnError)
		env := fs.String("e", "", "environment")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		scope, _, err := cliScope(ws, *env, nil)
		if err != nil {
			return err
		}
		for _, e := range scope.Entries() {
			if !e.Shadowed {
				fmt.Printf("%s=%s\t# %s\n", e.Name, e.Value, e.Source)
			}
		}
		return nil
	case "run":
		return runCLI(ws, rest)
	}
	return fmt.Errorf("unknown command %q (see turl -h)", cmd)
}

// parseWithName allows the item name before or after flags.
func parseWithName(fs *flag.FlagSet, args []string) (string, error) {
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if name == "" {
		return "", errors.New("missing request or flow name")
	}
	return name, nil
}

func cliScope(ws *spec.Workspace, envName string, sets []string) (*vars.Scope, *spec.State, error) {
	st := ws.LoadState()
	if envName != "" {
		st.Env = envName
	}
	env, err := ws.LoadEnv(st.Env)
	if err != nil {
		return nil, nil, err
	}
	over := map[string]string{}
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, nil, fmt.Errorf("-v expects k=v, got %q", s)
		}
		over[k] = v
	}
	return vars.NewScope(
		vars.Layer{Name: "cli", Vars: over},
		vars.Layer{Name: "state", Vars: st.Vars},
		vars.Layer{Name: "env:" + st.Env, Vars: env},
	), st, nil
}

func runCLI(ws *spec.Workspace, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	env := fs.String("e", "", "environment")
	quiet := fs.Bool("q", false, "no progress on stderr")
	filter := fs.String("jq", "", "jq filter applied to the final body")
	noSave := fs.Bool("no-save", false, "do not persist extracted variables")
	var sets multiFlag
	fs.Var(&sets, "v", "variable k=v (repeatable)")
	name, err := parseWithName(fs, args)
	if err != nil {
		return err
	}
	it, err := ws.Lookup(name)
	if err != nil {
		return err
	}
	scope, st, err := cliScope(ws, *env, sets)
	if err != nil {
		return err
	}

	// Ask for declared inputs not given with -v (only on a terminal).
	overrides := map[string]string{}
	for _, s := range sets {
		k, v, _ := strings.Cut(s, "=")
		overrides[k] = v
	}
	tty := isatty.IsTerminal(os.Stdin.Fd())
	in := bufio.NewReader(os.Stdin)
	for _, n := range runner.Inputs(ws, it) {
		if _, ok := overrides[n]; ok {
			continue
		}
		def, _, has := scope.Lookup(n)
		if !tty {
			if !has {
				return fmt.Errorf("input %q required (pass -v %s=VALUE)", n, n)
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "%s [%s]: ", n, def)
		line, _ := in.ReadString('\n')
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			overrides[n] = line
		} else if !has {
			return fmt.Errorf("input %q required", n)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	logf := func(format string, a ...any) {
		if !*quiet {
			fmt.Fprintln(os.Stderr, secrets.Redact(fmt.Sprintf(format, a...)))
		}
	}
	r := &runner.Runner{WS: ws, Scope: scope.Snapshot(), Emit: func(e runner.Event) {
		switch e.Kind {
		case runner.EvStepStart:
			logf("\x1b[1m%s\x1b[0m", e.Text)
		case runner.EvRequest, runner.EvResponse:
			logf("  %s", e.Text)
		case runner.EvWSOpen, runner.EvWSClose, runner.EvLog:
			logf("  %s", e.Text)
		case runner.EvWSSend:
			logf("  → %s", strings.TrimSpace(string(e.Msg.Data)))
		case runner.EvWSRecv:
			logf("  ← %s", strings.TrimSpace(string(e.Msg.Data)))
		case runner.EvExtract:
			for k, v := range e.Vars {
				logf("  ✓ %s = %s", k, v)
				st.Vars[k] = v
			}
		}
	}}
	for k, v := range overrides {
		if _, isInput := sliceSet(runner.Inputs(ws, it))[k]; isInput {
			st.Vars[k] = v
		}
	}
	res, runErr := r.Run(ctx, it, overrides)
	if !*noSave {
		if err := ws.SaveState(st); err != nil {
			logf("warning: saving state: %v", err)
		}
	}
	if res != nil {
		body := res.Body
		if res.WS {
			var parts []string
			for _, b := range res.Received() {
				parts = append(parts, string(b))
			}
			body = []byte(strings.Join(parts, "\n"))
		}
		if *filter != "" {
			vals, _ := jq.Decode(body)
			var out []any
			for _, v := range vals {
				o, err := jq.Eval(*filter, v, jq.Env{Status: res.StatusCode})
				if err != nil {
					return fmt.Errorf("jq: %w", err)
				}
				out = append(out, o...)
			}
			fmt.Println(jq.Format(out))
		} else if len(body) > 0 {
			os.Stdout.Write(body)
			if body[len(body)-1] != '\n' && isatty.IsTerminal(os.Stdout.Fd()) {
				fmt.Println()
			}
		}
	}
	return runErr
}

func sliceSet(s []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range s {
		m[v] = true
	}
	return m
}

// addCLI registers a request from a curl command:
//
//	turl add --collection ai 'curl -X POST https://api/v1/uploads -d ...'
//	turl add -c ai curl -X POST https://api/v1/uploads -d ...
//	pbpaste | turl add -c ai
func addCLI(ws *spec.Workspace, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	var collection, name string
	fs.StringVar(&collection, "collection", "", "collection (sub-folder of requests/)")
	fs.StringVar(&collection, "c", "", "shorthand for --collection")
	fs.StringVar(&name, "name", "", "request name (default: derived from method and URL)")
	fs.StringVar(&name, "n", "", "shorthand for --name")
	force := fs.Bool("f", false, "overwrite an existing request")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: turl add [--collection NAME] [--name NAME] [-f] CURL...")
		fs.PrintDefaults()
	}
	// Our flags must come before the curl command; everything after the
	// first non-flag (usually "curl") belongs to curl.
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	var req *spec.Request
	var err error
	switch {
	case len(rest) == 0:
		if isatty.IsTerminal(os.Stdin.Fd()) {
			fs.Usage()
			return errors.New("missing curl command")
		}
		b, rerr := io.ReadAll(os.Stdin)
		if rerr != nil {
			return rerr
		}
		req, err = curl.Import(string(b))
	case len(rest) == 1 && strings.ContainsAny(strings.TrimSpace(rest[0]), " \t\n"):
		// One quoted string holding the whole command.
		cmd := strings.TrimSpace(rest[0])
		if !strings.HasPrefix(cmd, "curl ") {
			cmd = "curl " + cmd
		}
		req, err = curl.Import(cmd)
	default:
		if rest[0] == "curl" || strings.HasSuffix(rest[0], "/curl") {
			rest = rest[1:]
		}
		req, err = curl.ImportArgs(rest)
	}
	if err != nil {
		return err
	}

	explicitName := name != ""
	if !explicitName {
		name = curl.SuggestName(req)
	}
	if collection != "" {
		name = strings.Trim(collection, "/") + "/" + name
	}
	if err := spec.ValidName(name); err != nil {
		return err
	}
	if !*force {
		base := name
		for i := 2; ; i++ {
			if _, err := os.Stat(ws.RequestPath(name)); os.IsNotExist(err) {
				break
			}
			if explicitName {
				return fmt.Errorf("%s already exists (use -f to overwrite)", name)
			}
			name = fmt.Sprintf("%s-%d", base, i)
		}
	}
	req.Name = name
	if err := ws.SaveRequest(req); err != nil {
		return err
	}
	fmt.Printf("added %s %s\n  %s\n", req.MethodOrDefault(), name, ws.RequestPath(name))
	return nil
}

func listCLI(ws *spec.Workspace) error {
	items := ws.Items()
	describe := func(it spec.Item) (string, string) {
		switch {
		case it.Err != nil:
			return "?", "error: " + it.Err.Error()
		case it.Flow != nil:
			return "FLOW", fmt.Sprintf("%d steps", len(it.Flow.Steps))
		}
		return it.Request.MethodOrDefault(), it.Request.URL
	}
	groups := map[string][]spec.Item{}
	first := true
	for _, it := range items {
		if it.Kind == spec.KindFlow {
			if first {
				fmt.Println("FLOWS")
				first = false
			}
			kind, desc := describe(it)
			fmt.Printf("  %-6s %-32s %s\n", kind, it.Name, desc)
			continue
		}
		c, _ := spec.SplitCollection(it.Name)
		groups[c] = append(groups[c], it)
	}
	colls := ws.Collections()
	for c := range groups {
		if c != "" && !ws.CollectionExists(c) {
			colls = append(colls, c)
		}
	}
	if len(groups[""]) > 0 {
		colls = append(colls, "")
	}
	for _, c := range colls {
		fmt.Printf("%s (%d)\n", spec.CollectionLabel(c), len(groups[c]))
		for _, it := range groups[c] {
			kind, desc := describe(it)
			_, short := spec.SplitCollection(it.Name)
			fmt.Printf("  %-6s %-32s %s\n", kind, short, desc)
		}
	}
	return nil
}

func collectionCLI(ws *spec.Workspace, args []string) error {
	sub := "ls"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "ls", "list":
		for _, c := range append(ws.Collections(), "") {
			fmt.Printf("%-24s %d requests\n", spec.CollectionLabel(c), len(ws.RequestsIn(c)))
		}
	case "new", "add":
		if len(args) != 1 {
			return errors.New("usage: turl collection new NAME")
		}
		if err := ws.NewCollection(args[0]); err != nil {
			return err
		}
		fmt.Println("created", ws.CollectionPath(args[0]))
	case "rename", "mv":
		if len(args) != 2 {
			return errors.New("usage: turl collection rename OLD NEW")
		}
		old, err := spec.ParseCollectionName(args[0])
		if err != nil {
			return err
		}
		flows, err := ws.RenameCollection(old, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("renamed %s to %s (%d flows updated)\n", old, args[1], flows)
	case "rm", "delete":
		fs := flag.NewFlagSet("rm", flag.ContinueOnError)
		force := fs.Bool("f", false, "delete even if it holds requests")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: turl collection rm [-f] NAME")
		}
		name, err := spec.ParseCollectionName(fs.Arg(0))
		if err != nil {
			return err
		}
		if n := len(ws.RequestsIn(name)); n > 0 && !*force {
			return fmt.Errorf("collection %s holds %d requests; use -f to delete them too", name, n)
		}
		if err := ws.DeleteCollection(name); err != nil {
			return err
		}
		fmt.Println("deleted", name)
	default:
		return fmt.Errorf("unknown subcommand %q (ls, new, rename, rm)", sub)
	}
	return nil
}

func secretCLI(store *secrets.Store, args []string) error {
	sub := "ls"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "ls", "list":
		infos, err := store.List()
		if err != nil {
			return err
		}
		if len(infos) == 0 {
			fmt.Printf("no secrets defined in %s\n", store.Path())
		}
		for _, in := range infos {
			state := "not fetched"
			if in.Cached {
				state = fmt.Sprintf("cached %s ago (ttl %s)", in.Age.Round(time.Second), in.TTL)
			}
			fmt.Printf("%-24s %-36s $ %s\n", in.Name, state, in.Command)
		}
	case "get":
		if len(args) != 1 {
			return errors.New("usage: turl secret get NAME")
		}
		v, err := store.Get(args[0])
		if err != nil {
			return err
		}
		fmt.Println(v)
	case "refresh":
		if err := store.Refresh(args...); err != nil {
			return err
		}
		fmt.Println("refreshed")
	case "clear":
		if err := store.Clear(); err != nil {
			return err
		}
		fmt.Println("cleared", store.CacheDir())
	default:
		return fmt.Errorf("unknown subcommand %q (ls, get, refresh, clear)", sub)
	}
	return nil
}
