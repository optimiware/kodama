package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/optimiware/kodema/internal/docs"
)

// runDocs dispatches `kodema docs [topic | --list | --all | --instructions | install ...]`.
// All subcommands are strictly offline and read the documentation embedded in
// this kodema binary.
func runDocs(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) > 0 && args[0] == "install" {
		return runDocsInstall(ctx, config, args[1:], ports, deps)
	}
	if len(args) > 0 && args[0] == "help" {
		return runHelp(ports.Out, ports.Err, append([]string{"docs"}, args[1:]...))
	}
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"docs"})
	}

	var (
		listFlag         bool
		allFlag          bool
		instructionsFlag bool
		outputFlag       string
	)
	spec := newArgSpec()
	spec.boolFlag(&listFlag, "--list")
	spec.boolFlag(&allFlag, "--all")
	spec.boolFlag(&instructionsFlag, "--instructions")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}

	modeCount := 0
	if listFlag {
		modeCount++
	}
	if allFlag {
		modeCount++
	}
	if instructionsFlag {
		modeCount++
	}
	if len(positionals) > 0 {
		modeCount++
	}
	if modeCount > 1 {
		return usage(ports.Err, "specify at most one of <topic>, --list, --all, or --instructions")
	}
	if len(positionals) > 1 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[1]))
	}

	switch {
	case instructionsFlag:
		text := docs.MCPInstructions()
		if format == OutputJSON {
			if err := json.NewEncoder(ports.Out).Encode(map[string]string{"instructions": text}); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
		fmt.Fprintln(ports.Out, text)
		return ExitSuccess

	case allFlag:
		if format == OutputJSON {
			payload := struct {
				Topics []docs.Topic `json:"topics"`
			}{Topics: docs.Topics()}
			if err := json.NewEncoder(ports.Out).Encode(payload); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
		if _, err := ports.Out.Write([]byte(docs.AllMarkdown())); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess

	case len(positionals) == 1:
		topic, ok := docs.Lookup(positionals[0])
		if !ok {
			return usage(ports.Err, fmt.Sprintf("unknown docs topic %q (run 'kodema docs --list' to see available topics)", positionals[0]))
		}
		if format == OutputJSON {
			if err := json.NewEncoder(ports.Out).Encode(topic); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
		if _, err := ports.Out.Write([]byte(topic.Content)); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess

	default:
		return runDocsList(ports, format)
	}
}

func runDocsList(ports Ports, format OutputFormat) int {
	type topicSummary struct {
		Slug        string `json:"slug"`
		URI         string `json:"uri"`
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	topics := docs.Topics()
	items := make([]topicSummary, len(topics))
	for index, topic := range topics {
		items[index] = topicSummary{
			Slug:        topic.Slug,
			URI:         topic.URI,
			Title:       topic.Title,
			Description: topic.Description,
		}
	}
	if format == OutputJSON {
		payload := struct {
			Topics []topicSummary `json:"topics"`
		}{Topics: items}
		if err := json.NewEncoder(ports.Out).Encode(payload); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOPIC\tURI\tDESCRIPTION")
	for _, item := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", item.Slug, item.URI, item.Description)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

// runDocsInstall writes the embedded Kodema documentation as a portable Agent
// Skill (`SKILL.md` plus `references/<topic>.md`) into one or more harness
// skill directories.
func runDocsInstall(_ context.Context, config Config, args []string, ports Ports, _ CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"docs", "install"})
	}
	var (
		targetFlag string
		destFlag   string
		outputFlag string
	)
	spec := newArgSpec()
	spec.stringFlag(&targetFlag, "--target")
	spec.stringFlag(&destFlag, "--dest", "--dir")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 0 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[0]))
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
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
		return usage(ports.Err, err.Error())
	}
	if ports.Files == nil {
		return failure(ports.Err, fmt.Errorf("no filesystem is configured"))
	}
	written, err := writeKodemaDocsSkill(ports.Files, skillDirs)
	if err != nil {
		return failure(ports.Err, err)
	}

	if format == OutputJSON {
		payload := map[string]any{
			"skill":  "kodema",
			"target": target,
			"dest":   dest,
			"files":  written,
		}
		if err := json.NewEncoder(ports.Out).Encode(payload); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	for _, dir := range skillDirs {
		fmt.Fprintf(ports.Out, "installed kodema skill in %s\n", filepath.Join(dir, "kodema"))
	}
	return ExitSuccess
}

// skillTargetDirs maps a harness target name (agents, claude, opencode, all) to
// the destination skill root directories under dest.
func skillTargetDirs(target, dest string) ([]string, error) {
	base := filepath.Clean(dest)
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "", "agents":
		return []string{filepath.Join(base, ".agents", "skills")}, nil
	case "claude":
		return []string{filepath.Join(base, ".claude", "skills")}, nil
	case "opencode":
		return []string{filepath.Join(base, ".opencode", "skills")}, nil
	case "all":
		return []string{
			filepath.Join(base, ".agents", "skills"),
			filepath.Join(base, ".claude", "skills"),
			filepath.Join(base, ".opencode", "skills"),
		}, nil
	default:
		return nil, fmt.Errorf("unknown skill target %q; want agents, claude, opencode, or all", target)
	}
}

// writeKodemaDocsSkill materializes the embedded Kodema product documentation
// as a portable Agent Skill (`kodema/SKILL.md` + `kodema/references/<slug>.md`)
// in each directory of skillDirs.
func writeKodemaDocsSkill(files FileSystem, skillDirs []string) ([]string, error) {
	skillDoc := []byte(docs.SkillDocument())
	topics := docs.Topics()
	written := make([]string, 0, len(skillDirs)*(len(topics)+1))
	for _, dir := range skillDirs {
		root := filepath.Join(dir, "kodema")
		entryPath := filepath.Join(root, "SKILL.md")
		if err := files.ReplaceFile(entryPath, skillDoc, 0o644); err != nil {
			return nil, fmt.Errorf("cannot write %s: %w", entryPath, err)
		}
		written = append(written, entryPath)
		for _, topic := range topics {
			refPath := filepath.Join(root, "references", topic.Slug+".md")
			if err := files.ReplaceFile(refPath, []byte(topic.Content), 0o644); err != nil {
				return nil, fmt.Errorf("cannot write %s: %w", refPath, err)
			}
			written = append(written, refPath)
		}
	}
	return written, nil
}
