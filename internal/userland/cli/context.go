package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/optimiware/kodema/internal/docs"
	"github.com/optimiware/kodema/internal/userland/skill"
)

const (
	contextAgentsStartMarker = "<!-- kodema:context:start -->"
	contextAgentsEndMarker   = "<!-- kodema:context:end -->"
	contextManifestRelPath   = ".kodema/context/.manifest.json"
	maxContextWalkDepth      = 6
)

// runContext dispatches `kodema context <pull|list|show>`.
func runContext(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "context requires a subcommand (pull|list|show)")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return runHelp(ports.Out, ports.Err, append([]string{"context"}, args[1:]...))
	}
	switch args[0] {
	case "pull":
		return runContextPull(ctx, config, args[1:], ports, deps)
	case "list":
		return runContextList(ctx, config, args[1:], ports)
	case "show":
		return runContextShow(ctx, config, args[1:], ports)
	default:
		return usage(ports.Err, fmt.Sprintf("unknown context subcommand %q", args[0]))
	}
}

type contextRepoSpec struct {
	Label string
	URL   string
	Ref   string
}

type contextExtraFile struct {
	Body []byte
	Mode fs.FileMode
}

type loadedContextSkill struct {
	Name        skill.Name
	SourcePath  string
	Description string
	Body        []byte
	ExtraFiles  map[string]contextExtraFile
	Repo        string
}

type loadedContextDoc struct {
	RelPath string
	Body    []byte
}

type loadedContextRepo struct {
	Skills []loadedContextSkill
	Docs   []loadedContextDoc
}

type contextPullOptions struct {
	Format      OutputFormat
	Target      string
	Dest        string
	SkillDirs   []string
	GitBaseURL  string
	Repos       []contextRepoSpec
	AgentsMD    string
	IncludeDocs bool
}

type pulledContextRepo struct {
	Spec   contextRepoSpec
	Commit string
	Loaded loadedContextRepo
}

type contextSkillSummary struct {
	Name        string   `json:"name"`
	Source      string   `json:"source"`
	Description string   `json:"description"`
	Paths       []string `json:"paths"`
}

type contextDocSummary struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
	Dest string `json:"dest"`
}

type contextRepoSummary struct {
	Repo   string `json:"repo"`
	URL    string `json:"url"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit"`
	Skills int    `json:"skills"`
	Docs   int    `json:"docs"`
}

type contextPullResult struct {
	Repos           []contextRepoSummary
	Skills          []contextSkillSummary
	Docs            []contextDocSummary
	ContextDocsBase string
	AgentsMDPath    string
}

type contextManifest struct {
	Files []string `json:"files"`
}

// runContextPull clones one or more Git context repositories (typically
// `<tenant>/context` and `<tenant>/<project>/context` from `$KODEMA_GIT_URL`),
// validates every Markdown document and skill through internal/userland/skill
// (symlink-safe, bounded, secret-free), and materializes them into harness
// skill directories and `.kodema/context/`.
func runContextPull(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"context", "pull"})
	}
	opts, err := parseContextPullOptions(config, args, deps)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if deps.GitRun == nil {
		return failure(ports.Err, fmt.Errorf("git runner is not configured"))
	}
	if ports.Files == nil {
		return failure(ports.Err, fmt.Errorf("no filesystem is configured"))
	}

	// Phase 1: clone and validate EVERY repo before touching dest so a secret
	// leak, symlink, or network failure never leaves partial state behind.
	pulled, err := pullContextRepos(ctx, config, opts.Repos, deps)
	if err != nil {
		return failure(ports.Err, err)
	}

	// Phase 2: materialize validated skills, docs, and manifest reconciliation.
	result, err := materializeContext(opts, pulled, ports.Files)
	if err != nil {
		return failure(ports.Err, err)
	}

	if opts.AgentsMD != "" {
		agentsPath, updateErr := updateContextAgentsMD(opts, result, ports.Files)
		if updateErr != nil {
			return failure(ports.Err, updateErr)
		}
		result.AgentsMDPath = agentsPath
	}

	if err := renderContextPullResult(ports.Out, opts, result); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func parseContextPullOptions(config Config, args []string, deps CommandDeps) (contextPullOptions, error) {
	var (
		reposFlag    []string
		gitURLFlag   string
		refFlag      string
		destFlag     string
		targetFlag   string
		agentsMDFlag string
		includeDocs  bool
		outputFlag   string
	)
	spec := newArgSpec()
	spec.listFlag(&reposFlag, "--repo", "-r")
	spec.stringFlag(&gitURLFlag, "--git-url")
	spec.stringFlag(&refFlag, "--ref")
	spec.stringFlag(&destFlag, "--dest", "--dir")
	spec.stringFlag(&targetFlag, "--target")
	spec.stringFlag(&agentsMDFlag, "--agents-md")
	spec.boolFlag(&includeDocs, "--include-kodema-docs")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return contextPullOptions{}, err
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return contextPullOptions{}, err
	}

	rawRepos := make([]string, 0, len(reposFlag)+len(positionals))
	rawRepos = append(rawRepos, reposFlag...)
	rawRepos = append(rawRepos, positionals...)
	if len(rawRepos) == 0 {
		return contextPullOptions{}, fmt.Errorf("context pull requires at least one --repo <name> (for example --repo context)")
	}

	defaultRef := strings.TrimSpace(refFlag)
	if defaultRef != "" {
		if err := validateContextRef(defaultRef); err != nil {
			return contextPullOptions{}, err
		}
	}

	target := strings.ToLower(strings.TrimSpace(targetFlag))
	if target == "" {
		target = "agents"
	}
	dest := strings.TrimSpace(destFlag)
	if dest == "" {
		dest = "."
	}
	skillDirs, err := skillTargetDirs(target, dest)
	if err != nil {
		return contextPullOptions{}, err
	}

	gitBaseURL := resolveContextGitBaseURL(gitURLFlag, config, deps)
	repos := make([]contextRepoSpec, 0, len(rawRepos))
	seenLabels := make(map[string]bool, len(rawRepos))
	for _, raw := range rawRepos {
		parsed, parseErr := parseContextRepoSpec(raw, gitBaseURL, defaultRef)
		if parseErr != nil {
			return contextPullOptions{}, parseErr
		}
		if seenLabels[parsed.Label] {
			return contextPullOptions{}, fmt.Errorf("duplicate context repository %q", parsed.Label)
		}
		seenLabels[parsed.Label] = true
		repos = append(repos, parsed)
	}

	return contextPullOptions{
		Format:      format,
		Target:      target,
		Dest:        dest,
		SkillDirs:   skillDirs,
		GitBaseURL:  gitBaseURL,
		Repos:       repos,
		AgentsMD:    strings.TrimSpace(agentsMDFlag),
		IncludeDocs: includeDocs,
	}, nil
}

func pullContextRepos(ctx context.Context, config Config, repos []contextRepoSpec, deps CommandDeps) ([]pulledContextRepo, error) {
	stageRoot, err := os.MkdirTemp("", "kodema-context-*")
	if err != nil {
		return nil, fmt.Errorf("cannot create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stageRoot) }()
	if resolved, evalErr := filepath.EvalSymlinks(stageRoot); evalErr == nil {
		stageRoot = resolved
	}

	pulled := make([]pulledContextRepo, 0, len(repos))
	for index, repoSpec := range repos {
		repoDir := filepath.Join(stageRoot, fmt.Sprintf("repo-%d", index))
		gitPrefix := contextGitConfigArgs(repoSpec.URL, config, deps)
		commit, cloneErr := cloneContextRepo(ctx, deps.GitRun, gitPrefix, repoSpec, repoDir)
		if cloneErr != nil {
			return nil, fmt.Errorf("pull context %q: %w", repoSpec.Label, cloneErr)
		}
		loaded, loadErr := loadContextRepo(ctx, repoDir, repoSpec.Label)
		if loadErr != nil {
			return nil, fmt.Errorf("validate context %q: %w", repoSpec.Label, loadErr)
		}
		pulled = append(pulled, pulledContextRepo{
			Spec:   repoSpec,
			Commit: commit,
			Loaded: loaded,
		})
	}
	return pulled, nil
}

func materializeContext(opts contextPullOptions, pulled []pulledContextRepo, files FileSystem) (contextPullResult, error) {
	// Merge skills in repo declaration order: later repos override earlier repos
	// on colliding skill slugs (e.g. project/context overrides company context).
	mergedSkills := make(map[skill.Name]loadedContextSkill)
	for _, entry := range pulled {
		for _, s := range entry.Loaded.Skills {
			mergedSkills[s.Name] = s
		}
	}
	skillNames := slices.Sorted(maps.Keys(mergedSkills))

	destBase := filepath.Clean(opts.Dest)
	contextDocsBase := filepath.Join(destBase, ".kodema", "context")
	manifestPath := filepath.Join(destBase, filepath.FromSlash(contextManifestRelPath))
	previousManaged := readContextManifest(files, manifestPath)

	result := contextPullResult{
		Repos:           make([]contextRepoSummary, 0, len(pulled)),
		Skills:          make([]contextSkillSummary, 0, len(skillNames)+1),
		Docs:            make([]contextDocSummary, 0),
		ContextDocsBase: contextDocsBase,
	}
	writtenRel := make(map[string]bool)
	recordWritten := func(fullPath string) {
		if rel, ok := safeRelPathUnder(destBase, fullPath); ok {
			writtenRel[rel] = true
		}
	}

	if opts.IncludeDocs {
		if _, overridden := mergedSkills[skill.Name("kodema")]; !overridden {
			written, writeErr := writeKodemaDocsSkill(files, opts.SkillDirs)
			if writeErr != nil {
				return contextPullResult{}, writeErr
			}
			for _, p := range written {
				recordWritten(p)
			}
			result.Skills = append(result.Skills, contextSkillSummary{
				Name:        "kodema",
				Source:      "embedded",
				Description: docs.SkillDescription,
				Paths:       written,
			})
		}
	}

	for _, name := range skillNames {
		item := mergedSkills[name]
		rendered := ensureSkillFrontmatter(item.Name, item.Description, item.Body)
		extraKeys := slices.Sorted(maps.Keys(item.ExtraFiles))
		paths := make([]string, 0, len(opts.SkillDirs)*(1+len(extraKeys)))
		for _, dir := range opts.SkillDirs {
			skillRoot := filepath.Join(dir, string(item.Name))
			entryPath := filepath.Join(skillRoot, "SKILL.md")
			if err := files.ReplaceFile(entryPath, rendered, 0o644); err != nil {
				return contextPullResult{}, fmt.Errorf("cannot write %s: %w", entryPath, err)
			}
			paths = append(paths, entryPath)
			recordWritten(entryPath)

			for _, rel := range extraKeys {
				extra := item.ExtraFiles[rel]
				extraPath := filepath.Join(skillRoot, filepath.FromSlash(rel))
				if err := files.ReplaceFile(extraPath, extra.Body, extra.Mode); err != nil {
					return contextPullResult{}, fmt.Errorf("cannot write %s: %w", extraPath, err)
				}
				paths = append(paths, extraPath)
				recordWritten(extraPath)
			}
		}
		result.Skills = append(result.Skills, contextSkillSummary{
			Name:        string(item.Name),
			Source:      item.Repo,
			Description: item.Description,
			Paths:       paths,
		})
	}
	slices.SortFunc(result.Skills, func(a, b contextSkillSummary) int {
		return strings.Compare(a.Name, b.Name)
	})

	for _, entry := range pulled {
		result.Repos = append(result.Repos, contextRepoSummary{
			Repo:   entry.Spec.Label,
			URL:    entry.Spec.URL,
			Ref:    entry.Spec.Ref,
			Commit: entry.Commit,
			Skills: len(entry.Loaded.Skills),
			Docs:   len(entry.Loaded.Docs),
		})
		for _, doc := range entry.Loaded.Docs {
			destPath := filepath.Join(contextDocsBase, filepath.FromSlash(entry.Spec.Label), filepath.FromSlash(doc.RelPath))
			if err := files.ReplaceFile(destPath, doc.Body, 0o644); err != nil {
				return contextPullResult{}, fmt.Errorf("cannot write %s: %w", destPath, err)
			}
			recordWritten(destPath)
			result.Docs = append(result.Docs, contextDocSummary{
				Repo: entry.Spec.Label,
				Path: doc.RelPath,
				Dest: destPath,
			})
		}
	}

	// Remove files previously materialized by `kodema context pull` that are no
	// longer present, leaving hand-authored files outside the manifest untouched.
	for _, oldRel := range previousManaged {
		if writtenRel[oldRel] {
			continue
		}
		stalePath := filepath.Join(destBase, filepath.FromSlash(oldRel))
		if err := files.Remove(stalePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return contextPullResult{}, fmt.Errorf("cannot remove stale context file %s: %w", stalePath, err)
		}
		pruneEmptyManagedDirs(filepath.Dir(stalePath), destBase)
	}

	if err := writeContextManifest(files, manifestPath, slices.Sorted(maps.Keys(writtenRel))); err != nil {
		return contextPullResult{}, err
	}
	return result, nil
}

func readContextManifest(files FileSystem, manifestPath string) []string {
	data, err := files.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	var manifest contextManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil
	}
	clean := make([]string, 0, len(manifest.Files))
	for _, entry := range manifest.Files {
		if isSafeManagedRelPath(entry) {
			clean = append(clean, entry)
		}
	}
	return clean
}

func writeContextManifest(files FileSystem, manifestPath string, relPaths []string) error {
	if relPaths == nil {
		relPaths = []string{}
	}
	data, err := json.MarshalIndent(contextManifest{Files: relPaths}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := files.ReplaceFile(manifestPath, data, 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", manifestPath, err)
	}
	return nil
}

func safeRelPathUnder(base, target string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil {
		return "", false
	}
	slashed := filepath.ToSlash(rel)
	if !isSafeManagedRelPath(slashed) {
		return "", false
	}
	return slashed, true
}

func isSafeManagedRelPath(rel string) bool {
	if rel == "" || rel == "." || strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
		return false
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// pruneEmptyManagedDirs removes empty directories left behind on real disk
// after removing stale managed files, stopping at managed roots (.agents/skills,
// .claude/skills, .opencode/skills, .kodema/context) and never touching destBase.
func pruneEmptyManagedDirs(dir, destBase string) {
	stops := map[string]bool{
		filepath.Clean(destBase):                       true,
		filepath.Join(destBase, ".agents", "skills"):   true,
		filepath.Join(destBase, ".claude", "skills"):   true,
		filepath.Join(destBase, ".opencode", "skills"): true,
		filepath.Join(destBase, ".kodema", "context"):  true,
		filepath.Join(destBase, ".kodema"):             true,
		filepath.Join(destBase, ".agents"):             true,
		filepath.Join(destBase, ".claude"):             true,
		filepath.Join(destBase, ".opencode"):           true,
		string(filepath.Separator):                     true,
		".":                                            true,
	}
	cur := filepath.Clean(dir)
	for !stops[cur] {
		if err := os.Remove(cur); err != nil {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
}

func updateContextAgentsMD(opts contextPullOptions, result contextPullResult, files FileSystem) (string, error) {
	agentsMDPath := opts.AgentsMD
	if !filepath.IsAbs(agentsMDPath) && opts.Dest != "." {
		agentsMDPath = filepath.Join(filepath.Clean(opts.Dest), agentsMDPath)
	}
	targetPath := resolveAgentsMDWritePath(agentsMDPath)

	var existing []byte
	if data, readErr := files.ReadFile(targetPath); readErr == nil {
		existing = data
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return "", fmt.Errorf("cannot read %s: %w", targetPath, readErr)
	}

	var block strings.Builder
	block.WriteString(contextAgentsStartMarker)
	block.WriteString("\n## Kodema Context\n\n")
	block.WriteString("Pulled context repositories:\n")
	for _, repo := range result.Repos {
		fmt.Fprintf(&block, "- `%s` (`%s`)\n", repo.Repo, shortCommit(repo.Commit))
	}
	if len(result.Docs) > 0 {
		block.WriteString("\nContext documents:\n")
		for _, doc := range result.Docs {
			fmt.Fprintf(&block, "- `%s`\n", relativeDisplayPath(opts.Dest, doc.Dest))
		}
	}
	if len(result.Skills) > 0 {
		fmt.Fprintf(&block, "\nInstalled skills (`%s`):\n", relativeDisplayPath(opts.Dest, opts.SkillDirs[0]))
		for _, s := range result.Skills {
			fmt.Fprintf(&block, "- `%s` — %s\n", s.Name, s.Description)
		}
	}
	block.WriteString(contextAgentsEndMarker)

	merged, mergeErr := mergeManagedAgentsMD(existing, block.String())
	if mergeErr != nil {
		return "", fmt.Errorf("cannot update %s: %w", targetPath, mergeErr)
	}
	if err := files.ReplaceFile(targetPath, merged, 0o644); err != nil {
		return "", fmt.Errorf("cannot write %s: %w", targetPath, err)
	}
	return agentsMDPath, nil
}

// resolveAgentsMDWritePath preserves user symlinks (such as AGENTS.md -> CLAUDE.md)
// by targeting the symlink destination instead of replacing the link with a
// regular file. Non-symlinks and paths absent from real disk (unit tests) are
// returned unchanged.
func resolveAgentsMDWritePath(p string) string {
	info, err := os.Lstat(p)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return p
	}
	if resolved, evalErr := filepath.EvalSymlinks(p); evalErr == nil {
		return resolved
	}
	if linkTarget, readErr := os.Readlink(p); readErr == nil {
		if filepath.IsAbs(linkTarget) {
			return filepath.Clean(linkTarget)
		}
		return filepath.Clean(filepath.Join(filepath.Dir(p), linkTarget))
	}
	return p
}

func renderContextPullResult(out io.Writer, opts contextPullOptions, result contextPullResult) error {
	if opts.Format == OutputJSON {
		payload := map[string]any{
			"target": opts.Target,
			"dest":   opts.Dest,
			"repos":  result.Repos,
			"skills": result.Skills,
			"docs":   result.Docs,
		}
		if opts.GitBaseURL != "" {
			payload["git_url"] = opts.GitBaseURL
		}
		if result.AgentsMDPath != "" {
			payload["agents_md"] = result.AgentsMDPath
		}
		return json.NewEncoder(out).Encode(payload)
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tCOMMIT\tSKILLS\tDOCS")
	for _, repo := range result.Repos {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", repo.Repo, shortCommit(repo.Commit), repo.Skills, repo.Docs)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(out, "installed %d skill(s) into %s and %d doc(s) into %s\n",
		len(result.Skills), strings.Join(opts.SkillDirs, ", "), len(result.Docs), result.ContextDocsBase)
	if result.AgentsMDPath != "" {
		fmt.Fprintf(out, "updated %s\n", result.AgentsMDPath)
	}
	return nil
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// runContextList validates and lists the skills and Markdown documents in a
// local context repository directory.
func runContextList(ctx context.Context, config Config, args []string, ports Ports) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"context", "list"})
	}
	var (
		dirFlag    string
		outputFlag string
	)
	spec := newArgSpec()
	spec.stringFlag(&dirFlag, "--dir", "-d")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 1 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[1]))
	}
	if len(positionals) == 1 && strings.TrimSpace(dirFlag) != "" {
		return usage(ports.Err, "specify either --dir or a positional directory, not both")
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}

	dir := strings.TrimSpace(dirFlag)
	if dir == "" && len(positionals) == 1 {
		dir = strings.TrimSpace(positionals[0])
	}
	if dir == "" {
		dir = "."
	}

	loaded, err := loadContextRepo(ctx, dir, filepath.Base(filepath.Clean(dir)))
	if err != nil {
		return failure(ports.Err, err)
	}

	type skillRow struct {
		Name        string `json:"name"`
		Path        string `json:"path"`
		Description string `json:"description"`
	}
	type docRow struct {
		Path string `json:"path"`
	}
	skillsOut := make([]skillRow, len(loaded.Skills))
	for i, s := range loaded.Skills {
		skillsOut[i] = skillRow{Name: string(s.Name), Path: s.SourcePath, Description: s.Description}
	}
	docsOut := make([]docRow, len(loaded.Docs))
	for i, d := range loaded.Docs {
		docsOut[i] = docRow{Path: d.RelPath}
	}

	if format == OutputJSON {
		payload := map[string]any{
			"dir":    dir,
			"skills": skillsOut,
			"docs":   docsOut,
		}
		if err := json.NewEncoder(ports.Out).Encode(payload); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tNAME\tPATH\tDESCRIPTION")
	for _, s := range skillsOut {
		fmt.Fprintf(tw, "skill\t%s\t%s\t%s\n", s.Name, s.Path, s.Description)
	}
	for _, d := range docsOut {
		fmt.Fprintf(tw, "doc\t-\t%s\t-\n", d.Path)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

// runContextShow validates and prints one named skill from a local context
// repository directory.
func runContextShow(ctx context.Context, config Config, args []string, ports Ports) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"context", "show"})
	}
	var (
		dirFlag    string
		outputFlag string
	)
	spec := newArgSpec()
	spec.stringFlag(&dirFlag, "--dir", "-d")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 {
		return usage(ports.Err, "context show requires exactly one skill name")
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	name, err := skill.ParseName(strings.TrimSpace(positionals[0]))
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	dir := strings.TrimSpace(dirFlag)
	if dir == "" {
		dir = "."
	}

	loaded, err := loadContextRepo(ctx, dir, filepath.Base(filepath.Clean(dir)))
	if err != nil {
		return failure(ports.Err, err)
	}
	for _, item := range loaded.Skills {
		if item.Name != name {
			continue
		}
		if format == OutputJSON {
			payload := map[string]any{
				"name":        string(item.Name),
				"path":        item.SourcePath,
				"description": item.Description,
				"content":     string(item.Body),
			}
			if err := json.NewEncoder(ports.Out).Encode(payload); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
		if _, err := ports.Out.Write(item.Body); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	return failure(ports.Err, fmt.Errorf("skill %q not found in %s", name, dir))
}

func resolveContextEnv(deps CommandDeps) func(string) string {
	if deps.Env != nil {
		return deps.Env
	}
	if deps.Login.Environment != nil {
		return deps.Login.Environment
	}
	return os.Getenv
}

func resolveContextGitBaseURL(flagVal string, config Config, deps CommandDeps) string {
	if trimmed := strings.TrimSpace(flagVal); trimmed != "" {
		return strings.TrimRight(trimmed, "/")
	}
	env := resolveContextEnv(deps)
	if trimmed := strings.TrimSpace(env("KODEMA_GIT_URL")); trimmed != "" {
		return strings.TrimRight(trimmed, "/")
	}
	if trimmed := strings.TrimSpace(config.ServerURL); trimmed != "" {
		return strings.TrimRight(trimmed, "/") + "/git"
	}
	return ""
}

func parseContextRepoSpec(raw, gitBaseURL, defaultRef string) (contextRepoSpec, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return contextRepoSpec{}, fmt.Errorf("context repository name is empty")
	}
	if strings.ContainsAny(trimmed, " \t\r\n") || hasControlRune(trimmed) {
		return contextRepoSpec{}, fmt.Errorf("invalid context repository %q", raw)
	}

	repoPart := trimmed
	ref := defaultRef
	if idx := strings.LastIndex(trimmed, "@"); idx >= 0 {
		// Split @ref only when it follows the repository path (not a URL userinfo).
		splitRef := false
		if schemeIdx := strings.Index(trimmed, "://"); schemeIdx < 0 {
			splitRef = true
		} else if idx > schemeIdx+3 {
			if afterSlash := strings.Index(trimmed[schemeIdx+3:], "/"); afterSlash >= 0 && idx > schemeIdx+3+afterSlash {
				splitRef = true
			}
		}
		if splitRef {
			repoPart = trimmed[:idx]
			ref = trimmed[idx+1:]
			if strings.TrimSpace(ref) == "" {
				return contextRepoSpec{}, fmt.Errorf("context repository %q has an empty @ref", raw)
			}
		}
	}
	if ref != "" {
		if err := validateContextRef(ref); err != nil {
			return contextRepoSpec{}, err
		}
	}
	if repoPart == "" || strings.HasPrefix(repoPart, "-") {
		return contextRepoSpec{}, fmt.Errorf("invalid context repository %q", raw)
	}

	// Explicit URL (https:// or file://).
	if strings.Contains(repoPart, "://") {
		parsed, err := validateContextGitURL(repoPart, "context repository URL")
		if err != nil {
			return contextRepoSpec{}, err
		}
		label, err := deriveContextRepoLabel(parsed.Path)
		if err != nil {
			return contextRepoSpec{}, fmt.Errorf("invalid context repository URL %q: %w", repoPart, err)
		}
		return contextRepoSpec{
			Label: label,
			URL:   repoPart,
			Ref:   ref,
		}, nil
	}

	// Tenant-relative repository path resolved against gitBaseURL.
	label, err := validateRelativeRepoPath(repoPart)
	if err != nil {
		return contextRepoSpec{}, err
	}
	if gitBaseURL == "" {
		return contextRepoSpec{}, fmt.Errorf("context repository %q requires --git-url or $KODEMA_GIT_URL (or --server)", repoPart)
	}
	if _, err := validateContextGitURL(gitBaseURL, "--git-url"); err != nil {
		return contextRepoSpec{}, err
	}
	return contextRepoSpec{
		Label: label,
		URL:   strings.TrimRight(gitBaseURL, "/") + "/" + label,
		Ref:   ref,
	}, nil
}

func validateContextGitURL(raw, label string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", label, raw, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "file" {
		return nil, fmt.Errorf("%s %q must use https or file scheme", label, raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%s %q must not contain credentials, query strings, or fragments", label, raw)
	}
	if scheme == "https" && parsed.Host == "" {
		return nil, fmt.Errorf("%s %q is missing a host", label, raw)
	}
	return parsed, nil
}

func validateRelativeRepoPath(repo string) (string, error) {
	if strings.HasPrefix(repo, "/") || strings.Contains(repo, "\\") || strings.ContainsAny(repo, "?#") {
		return "", fmt.Errorf("invalid context repository path %q", repo)
	}
	trimmed := strings.TrimSuffix(repo, ".git")
	segments := strings.Split(trimmed, "/")
	cleaned := make([]string, 0, len(segments))
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, "-") || strings.HasPrefix(seg, ".") {
			return "", fmt.Errorf("invalid context repository path %q", repo)
		}
		cleaned = append(cleaned, seg)
	}
	if len(cleaned) == 0 {
		return "", fmt.Errorf("invalid context repository path %q", repo)
	}
	return strings.Join(cleaned, "/"), nil
}

func deriveContextRepoLabel(urlPath string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(urlPath), "/")
	trimmed = strings.TrimPrefix(trimmed, "git/")
	trimmed = strings.TrimSuffix(trimmed, ".git")
	if trimmed == "" {
		return "", fmt.Errorf("repository path is empty")
	}
	segments := strings.Split(trimmed, "/")
	// Keep at most the last two clean segments so absolute file:///tmp/.../repo
	// paths produce a short readable label without parent traversal.
	if len(segments) > 2 {
		segments = segments[len(segments)-2:]
	}
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "-") {
			return "", fmt.Errorf("repository path segment %q is invalid", seg)
		}
	}
	return strings.Join(segments, "/"), nil
}

func validateContextRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	if strings.ContainsAny(ref, " \t\r\n\\~^:?*[") || hasControlRune(ref) {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	return nil
}

func hasControlRune(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// contextGitConfigArgs builds origin-scoped `-c` flags for workstation pulls
// against a Kodema HTTPS origin when the container runtime has not already
// injected GIT_CONFIG_COUNT. Never emits token material and never touches
// unrelated or file:// origins.
func contextGitConfigArgs(repoURL string, config Config, deps CommandDeps) []string {
	env := resolveContextEnv(deps)
	if strings.TrimSpace(env("GIT_CONFIG_COUNT")) != "" {
		return nil
	}
	target, err := ParseOrigin(repoURL)
	if err != nil {
		return nil
	}
	matchesKodema := false
	if strings.TrimSpace(config.ServerURL) != "" {
		if serverOrigin, parseErr := ParseOrigin(config.ServerURL); parseErr == nil && serverOrigin.Matches(target.String()) {
			matchesKodema = true
		}
	}
	if !matchesKodema && matchesTrustedOrigin(config, target.String()) {
		matchesKodema = true
	}
	if !matchesKodema {
		return nil
	}

	origin := target.Scheme + "://" + target.Host
	caFile := absIfNonEmpty(config.CAFile)
	credFile := absIfNonEmpty(config.CredentialFile)
	sessionFile := absIfNonEmpty(config.SessionFile)
	helperValue := buildGitCredentialHelper(deps.CommandPath, config.ServerURL, caFile, credFile, sessionFile, origin)

	args := []string{
		"-c", "credential." + origin + ".helper=",
		"-c", "credential." + origin + ".helper=" + helperValue,
	}
	if caFile != "" {
		args = append(args, "-c", "http."+origin+".sslCAInfo="+caFile)
	}
	return args
}

func absIfNonEmpty(p string) string {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return ""
	}
	if abs, err := filepath.Abs(trimmed); err == nil {
		return abs
	}
	return trimmed
}

func cloneContextRepo(
	ctx context.Context,
	gitRun func(context.Context, string, []string) (string, error),
	gitPrefix []string,
	repoSpec contextRepoSpec,
	repoDir string,
) (string, error) {
	runWithAuth := func(dir string, args ...string) (string, error) {
		full := make([]string, 0, len(gitPrefix)+len(args))
		full = append(full, gitPrefix...)
		full = append(full, args...)
		return gitRun(ctx, dir, full)
	}

	if repoSpec.Ref == "" {
		if _, err := runWithAuth("", "clone", "--quiet", "--depth", "1", "--", repoSpec.URL, repoDir); err != nil {
			return "", err
		}
	} else {
		if err := os.MkdirAll(repoDir, 0o700); err != nil {
			return "", err
		}
		if _, err := gitRun(ctx, repoDir, []string{"init", "--quiet"}); err != nil {
			return "", err
		}
		if _, err := gitRun(ctx, repoDir, []string{"remote", "add", "origin", "--", repoSpec.URL}); err != nil {
			return "", err
		}
		if _, err := runWithAuth(repoDir, "fetch", "--quiet", "--depth", "1", "origin", "--", repoSpec.Ref); err == nil {
			if _, coErr := gitRun(ctx, repoDir, []string{"checkout", "--quiet", "--detach", "FETCH_HEAD"}); coErr != nil {
				return "", coErr
			}
		} else {
			if _, fetchErr := runWithAuth(repoDir, "fetch", "--quiet", "origin"); fetchErr != nil {
				return "", fetchErr
			}
			resolved, resolveErr := resolveFetchedCommit(ctx, gitRun, repoDir, repoSpec.Ref)
			if resolveErr != nil {
				return "", fmt.Errorf("ref %q not found (%v)", repoSpec.Ref, err)
			}
			if _, coErr := gitRun(ctx, repoDir, []string{"checkout", "--quiet", "--detach", resolved}); coErr != nil {
				return "", coErr
			}
		}
	}

	commitOut, err := gitRun(ctx, repoDir, []string{"rev-parse", "--verify", "HEAD^{commit}"})
	if err != nil {
		return "", fmt.Errorf("resolve HEAD commit: %w", err)
	}
	commit := strings.TrimSpace(commitOut)
	if commit == "" || strings.ContainsAny(commit, " \t\r\n") {
		return "", fmt.Errorf("resolved HEAD commit %q is invalid", commit)
	}
	return commit, nil
}

func resolveFetchedCommit(
	ctx context.Context,
	gitRun func(context.Context, string, []string) (string, error),
	repoDir, ref string,
) (string, error) {
	for _, candidate := range []string{ref + "^{commit}", "refs/remotes/origin/" + ref + "^{commit}"} {
		if out, err := gitRun(ctx, repoDir, []string{"rev-parse", "--verify", "--quiet", candidate}); err == nil {
			if sha := strings.TrimSpace(out); sha != "" && !strings.ContainsAny(sha, " \t\r\n") {
				return sha, nil
			}
		}
	}
	return "", fmt.Errorf("ref %q not found", ref)
}

func relativeDisplayPath(base, target string) string {
	cleanBase := filepath.Clean(base)
	cleanTarget := filepath.Clean(target)
	if rel, err := filepath.Rel(cleanBase, cleanTarget); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(cleanTarget)
}

// loadContextRepo inspects a local context repository directory through a
// pinned os.Root handle and internal/userland/skill, refusing symlinks,
// oversized files, duplicate skill definitions, and any credential material.
func loadContextRepo(ctx context.Context, rootPath, repoLabel string) (loadedContextRepo, error) {
	if err := ctx.Err(); err != nil {
		return loadedContextRepo{}, err
	}
	root, absRoot, err := skill.OpenConfinedRoot(rootPath)
	if err != nil {
		return loadedContextRepo{}, err
	}
	defer func() { _ = root.Close() }()

	var result loadedContextRepo

	// 1. Load skills/ if present: both flat skills/<slug>.md (via skill.DirSource
	//    + skill.MarkdownLoader) and directory skills/<slug>/SKILL.md.
	if skillsInfo, statErr := root.Lstat("skills"); statErr == nil {
		if skillsInfo.Mode()&os.ModeSymlink != 0 {
			return loadedContextRepo{}, fmt.Errorf("skills directory is a symlink")
		}
		if !skillsInfo.IsDir() {
			return loadedContextRepo{}, fmt.Errorf("skills is not a directory")
		}
		skillsAbs := filepath.Join(absRoot, "skills")
		skillsLoaded, err := loadContextSkills(ctx, root, skillsAbs, repoLabel)
		if err != nil {
			return loadedContextRepo{}, err
		}
		result.Skills = skillsLoaded
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return loadedContextRepo{}, fmt.Errorf("stat skills: %w", statErr)
	}

	// 2. Load top-level *.md files and docs/**/*.md files.
	docsLoaded, err := loadContextMarkdownDocs(ctx, root)
	if err != nil {
		return loadedContextRepo{}, err
	}
	result.Docs = docsLoaded
	return result, nil
}

func loadContextSkills(ctx context.Context, repoRoot *os.Root, skillsAbs, repoLabel string) ([]loadedContextSkill, error) {
	dirSource, err := skill.NewDirSource(skillsAbs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dirSource.Close() }()
	loader, err := skill.NewLoader(dirSource)
	if err != nil {
		return nil, err
	}

	flatNames, err := loader.List(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[skill.Name]loadedContextSkill, len(flatNames))
	for _, name := range flatNames {
		loaded, loadErr := loader.Load(ctx, name)
		if loadErr != nil {
			return nil, loadErr
		}
		byName[name] = loadedContextSkill{
			Name:        loaded.Name,
			SourcePath:  path.Join("skills", loaded.Path),
			Description: extractSkillDescription(loaded.Name, loaded.Body),
			Body:        loaded.Body,
			Repo:        repoLabel,
		}
	}

	// Inspect skills/ entries directly to reject symlinks and load directory
	// skills (`skills/<slug>/SKILL.md` plus optional supporting files).
	skillsDir, err := repoRoot.Open("skills")
	if err != nil {
		return nil, fmt.Errorf("open skills: %w", err)
	}
	entries, err := skillsDir.ReadDir(-1)
	_ = skillsDir.Close()
	if err != nil {
		return nil, fmt.Errorf("read skills: %w", err)
	}
	for _, entry := range entries {
		entryName := entry.Name()
		if strings.HasPrefix(entryName, ".") {
			continue
		}
		rel := path.Join("skills", entryName)
		info, statErr := repoRoot.Lstat(rel)
		if statErr != nil {
			return nil, fmt.Errorf("stat %s: %w", rel, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("context entry %q is a symlink", rel)
		}
		if !info.IsDir() {
			continue
		}
		slug, parseErr := skill.ParseName(entryName)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid skill directory %q: %w", rel, parseErr)
		}
		entryFile := path.Join(rel, "SKILL.md")
		body, _, readErr := skill.ReadConfinedFile(repoRoot, entryFile)
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", entryFile, readErr)
		}
		candidate := skill.Skill{Name: slug, Path: entryFile, Body: body}
		if err := candidate.Validate(); err != nil {
			return nil, err
		}
		if skill.ContainsSecret(body) {
			return nil, fmt.Errorf("skill %q (%s) contains credential material", slug, entryFile)
		}
		if _, duplicate := byName[slug]; duplicate {
			return nil, fmt.Errorf("duplicate skill %q: both skills/%s.md and %s exist", slug, slug, entryFile)
		}
		extras, extraErr := collectSkillSupportingFiles(ctx, repoRoot, rel, slug)
		if extraErr != nil {
			return nil, extraErr
		}
		byName[slug] = loadedContextSkill{
			Name:        slug,
			SourcePath:  entryFile,
			Description: extractSkillDescription(slug, body),
			Body:        body,
			ExtraFiles:  extras,
			Repo:        repoLabel,
		}
	}

	names := slices.Sorted(maps.Keys(byName))
	skills := make([]loadedContextSkill, len(names))
	for i, name := range names {
		skills[i] = byName[name]
	}
	return skills, nil
}

// walkConfinedContextDir recursively traverses relDir under a pinned *os.Root,
// skipping dotfiles, refusing symlinks, and bounding nesting depth.
func walkConfinedContextDir(
	ctx context.Context,
	repoRoot *os.Root,
	relDir string,
	maxDepth int,
	visit func(relPath, subPath string, info fs.FileInfo) error,
) error {
	var walk func(curRel, subPrefix string, depth int) error
	walk = func(curRel, subPrefix string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > maxDepth {
			return fmt.Errorf("directory nesting under %q exceeds %d levels", curRel, maxDepth)
		}
		handle, err := repoRoot.Open(curRel)
		if err != nil {
			return fmt.Errorf("open %s: %w", curRel, err)
		}
		entries, err := handle.ReadDir(-1)
		_ = handle.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", curRel, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			childRel := name
			if curRel != "." {
				childRel = path.Join(curRel, name)
			}
			childSub := name
			if subPrefix != "" {
				childSub = path.Join(subPrefix, name)
			}
			info, statErr := repoRoot.Lstat(childRel)
			if statErr != nil {
				return fmt.Errorf("stat %s: %w", childRel, statErr)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("context entry %q is a symlink", childRel)
			}
			if info.IsDir() {
				if err := walk(childRel, childSub, depth+1); err != nil {
					return err
				}
				continue
			}
			if err := visit(childRel, childSub, info); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(relDir, "", 0)
}

func collectSkillSupportingFiles(ctx context.Context, repoRoot *os.Root, skillRelDir string, slug skill.Name) (map[string]contextExtraFile, error) {
	extras := make(map[string]contextExtraFile)
	err := walkConfinedContextDir(ctx, repoRoot, skillRelDir, maxContextWalkDepth, func(relPath, subPath string, info fs.FileInfo) error {
		if subPath == "SKILL.md" {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("skill %q file %q is not a regular file", slug, relPath)
		}
		body, mode, readErr := skill.ReadConfinedFile(repoRoot, relPath)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", relPath, readErr)
		}
		if skill.ContainsSecret(body) {
			return fmt.Errorf("skill %q supporting file %q contains credential material", slug, relPath)
		}
		outMode := fs.FileMode(0o644)
		if mode&0o111 != 0 {
			outMode = 0o755
		}
		extras[subPath] = contextExtraFile{Body: body, Mode: outMode}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(extras) == 0 {
		return nil, nil
	}
	return extras, nil
}

func loadContextMarkdownDocs(ctx context.Context, repoRoot *os.Root) ([]loadedContextDoc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var docsOut []loadedContextDoc

	rootHandle, err := repoRoot.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open context root: %w", err)
	}
	entries, err := rootHandle.ReadDir(-1)
	_ = rootHandle.Close()
	if err != nil {
		return nil, fmt.Errorf("read context root: %w", err)
	}

	readDoc := func(relPath string) error {
		body, _, readErr := skill.ReadConfinedFile(repoRoot, relPath)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", relPath, readErr)
		}
		if skill.ContainsSecret(body) {
			return fmt.Errorf("context document %q contains credential material", relPath)
		}
		docsOut = append(docsOut, loadedContextDoc{RelPath: relPath, Body: body})
		return nil
	}

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, statErr := repoRoot.Lstat(name)
		if statErr != nil {
			return nil, fmt.Errorf("stat %s: %w", name, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if strings.HasSuffix(strings.ToLower(name), ".md") || name == "docs" {
				return nil, fmt.Errorf("context entry %q is a symlink", name)
			}
			continue
		}
		if info.IsDir() {
			if name == "docs" {
				walkErr := walkConfinedContextDir(ctx, repoRoot, "docs", maxContextWalkDepth, func(relPath, _ string, fileInfo fs.FileInfo) error {
					if !fileInfo.Mode().IsRegular() || !strings.HasSuffix(relPath, ".md") {
						return nil
					}
					return readDoc(relPath)
				})
				if walkErr != nil {
					return nil, walkErr
				}
			}
			continue
		}
		if !info.Mode().IsRegular() || !strings.HasSuffix(name, ".md") {
			continue
		}
		if err := readDoc(name); err != nil {
			return nil, err
		}
	}

	slices.SortFunc(docsOut, func(a, b loadedContextDoc) int {
		return strings.Compare(a.RelPath, b.RelPath)
	})
	return docsOut, nil
}

// splitYAMLFrontmatter splits a leading YAML frontmatter block delimited by
// `---` lines from the Markdown body that follows.
func splitYAMLFrontmatter(body []byte) (frontmatter, rest string, ok bool) {
	normalized := strings.ReplaceAll(string(body), "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return "", normalized, false
	}
	after := normalized[4:]
	if end := strings.Index(after, "\n---\n"); end >= 0 {
		return after[:end], after[end+5:], true
	}
	if strings.HasSuffix(after, "\n---") {
		return after[:len(after)-4], "", true
	}
	return "", normalized, false
}

// extractFrontmatterDescription reads the `description` scalar from YAML
// frontmatter, supporting inline plain/quoted strings as well as folded (`>`)
// and literal (`|`) indented block scalars.
func extractFrontmatterDescription(fm string) (string, bool) {
	lines := strings.Split(fm, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		// Only match top-level keys (column 0).
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		value, found := strings.CutPrefix(line, "description:")
		if !found {
			continue
		}
		trimmed := strings.TrimSpace(value)
		if isYAMLBlockScalarHeader(trimmed) {
			var blockLines []string
			for j := i + 1; j < len(lines); j++ {
				next := lines[j]
				if strings.TrimSpace(next) == "" {
					if len(blockLines) > 0 {
						blockLines = append(blockLines, "")
					}
					continue
				}
				if !strings.HasPrefix(next, " ") && !strings.HasPrefix(next, "\t") {
					break
				}
				blockLines = append(blockLines, strings.TrimSpace(next))
			}
			desc := truncateOneLine(strings.Join(blockLines, " "), 200)
			if desc != "" {
				return desc, true
			}
			return "", false
		}
		desc := unquoteYAMLScalar(trimmed)
		if desc != "" {
			return truncateOneLine(desc, 200), true
		}
		return "", false
	}
	return "", false
}

func isYAMLBlockScalarHeader(val string) bool {
	if val == "" || (val[0] != '>' && val[0] != '|') {
		return false
	}
	rest := val[1:]
	// Strip optional YAML comment after whitespace (e.g. `> # comment`).
	if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		if !strings.HasPrefix(strings.TrimSpace(rest[idx:]), "#") {
			return false
		}
		rest = rest[:idx]
	}
	for _, r := range rest {
		if r != '-' && r != '+' && (r < '1' || r > '9') {
			return false
		}
	}
	return true
}

func unquoteYAMLScalar(val string) string {
	if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
		if unquoted, err := strconv.Unquote(val); err == nil {
			return strings.TrimSpace(unquoted)
		}
		return strings.TrimSpace(val[1 : len(val)-1])
	}
	if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
		return strings.TrimSpace(strings.ReplaceAll(val[1:len(val)-1], "''", "'"))
	}
	return strings.TrimSpace(val)
}

// extractSkillDescription returns the description from YAML frontmatter if
// present, or synthesizes a concise one-line description from the first prose
// paragraph or Markdown heading.
func extractSkillDescription(name skill.Name, body []byte) string {
	fm, text, hasFM := splitYAMLFrontmatter(body)
	if hasFM {
		if desc, ok := extractFrontmatterDescription(fm); ok {
			return desc
		}
	}

	var heading string
	var paraLines []string
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if len(paraLines) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			if heading == "" {
				heading = strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			}
			if len(paraLines) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			if len(paraLines) > 0 {
				break
			}
			continue
		}
		paraLines = append(paraLines, trimmed)
	}
	if len(paraLines) > 0 {
		return truncateOneLine(strings.Join(paraLines, " "), 200)
	}
	if heading != "" {
		return truncateOneLine(heading, 200)
	}
	return "Skill " + string(name)
}

func truncateOneLine(text string, limit int) string {
	joined := strings.Join(strings.Fields(text), " ")
	runes := []rune(joined)
	if len(runes) <= limit {
		return joined
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return strings.TrimSpace(string(runes[:limit-3])) + "..."
}

// ensureSkillFrontmatter ensures portable Agent Skill frontmatter (`name` +
// `description`) is present so skills are discovered automatically by OpenCode,
// Claude Code, and Codex. Existing frontmatter with a description is preserved
// verbatim; existing frontmatter missing a description receives the synthesized
// description.
func ensureSkillFrontmatter(name skill.Name, description string, body []byte) []byte {
	if fm, rest, ok := splitYAMLFrontmatter(body); ok {
		if _, hasDesc := extractFrontmatterDescription(fm); hasDesc {
			return body
		}
		lines := strings.Split(strings.TrimRight(fm, "\n"), "\n")
		replaced := false
		for i, line := range lines {
			if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && strings.HasPrefix(line, "description:") {
				lines[i] = "description: " + strconv.Quote(description)
				replaced = true
				break
			}
		}
		if !replaced {
			lines = append(lines, "description: "+strconv.Quote(description))
		}
		var out bytes.Buffer
		out.WriteString("---\n")
		out.WriteString(strings.Join(lines, "\n"))
		out.WriteString("\n---\n")
		out.WriteString(rest)
		if len(rest) > 0 && rest[len(rest)-1] != '\n' {
			out.WriteByte('\n')
		}
		return out.Bytes()
	}
	var out bytes.Buffer
	out.WriteString("---\n")
	out.WriteString("name: ")
	out.WriteString(string(name))
	out.WriteByte('\n')
	out.WriteString("description: ")
	out.WriteString(strconv.Quote(description))
	out.WriteString("\n---\n\n")
	out.Write(body)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func mergeManagedAgentsMD(existing []byte, managedBlock string) ([]byte, error) {
	if len(bytes.TrimSpace(existing)) == 0 {
		return []byte(managedBlock + "\n"), nil
	}
	text := strings.ReplaceAll(string(existing), "\r\n", "\n")
	startIdx := strings.Index(text, contextAgentsStartMarker)
	endIdx := strings.Index(text, contextAgentsEndMarker)
	if startIdx < 0 && endIdx < 0 {
		return []byte(strings.TrimRight(text, "\n") + "\n\n" + managedBlock + "\n"), nil
	}
	if startIdx < 0 || endIdx < 0 || endIdx < startIdx {
		return nil, fmt.Errorf("malformed Kodema context markers (%s / %s)", contextAgentsStartMarker, contextAgentsEndMarker)
	}
	endPos := endIdx + len(contextAgentsEndMarker)
	updated := text[:startIdx] + managedBlock + text[endPos:]
	if !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	return []byte(updated), nil
}
