package cli

import (
	"bytes"
	"context"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/heilingbrunner/clonezip/internal/common/repolist"
)

// migrateOptions holds the flags of the migrate command.
type migrateOptions struct {
	global *globalOptions

	In  string
	Out string
}

func newMigrateCmd(global *globalOptions) *cobra.Command {
	opts := &migrateOptions{global: global}

	cmd := &cobra.Command{
		Use:   "migrate <old.txt> <new.yaml>",
		Short: "Convert a legacy plain-text repo list into the current YAML format",
		Long: "Reads a plain-text repo list (one clone URL per line, with optional\n" +
			"decorative label lines) and writes the equivalent YAML list read by\n" +
			"backup and service. Decorative labels become YAML comments placed\n" +
			"above the URLs that followed them; blank lines and existing '#'\n" +
			"comments are dropped, since YAML carries its own comment syntax.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.In, opts.Out = args[0], args[1]
			return runMigrate(cmd.Context(), opts)
		},
	}

	return cmd
}

func runMigrate(ctx context.Context, opts *migrateOptions) error {
	data, err := os.ReadFile(opts.In)
	if err != nil {
		return usagef("read %s: %w", opts.In, err)
	}

	out, urls, labels := convertLegacyList(data)

	if err := os.WriteFile(opts.Out, out, 0o644); err != nil {
		return usagef("write %s: %w", opts.Out, err)
	}

	unit := "repositories"
	if urls == 1 {
		unit = "repository"
	}
	p := printer{w: os.Stdout}
	p.printf("clonezip: wrote %s (%d %s, %d label(s) converted to comments)\n", opts.Out, urls, unit, labels)

	// Parsed back immediately so a bad line is caught here rather than at the
	// first real backup run.
	list, err := repolist.Parse(out)
	if err != nil {
		return usagef("the converted list does not parse: %w", err)
	}
	for _, issue := range list.Issues {
		p.printf("  %s\n", issue)
	}
	if list.HasErrors() {
		p.printf("clonezip: fix the problem(s) above before using %s\n", opts.Out)
	}
	return nil
}

// convertLegacyList renders a legacy plain-text list as YAML. A run of label
// lines immediately preceding a URL becomes that entry's head comment; label
// lines are never tied to more than the one URL that follows them, since the
// old format never reliably tied them to more than that either.
func convertLegacyList(data []byte) (out []byte, urls, labels int) {
	entries := repolist.ParseLegacyText(data)

	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}

	var pending []string
	for _, e := range entries {
		if !e.IsURL {
			pending = append(pending, e.Text)
			labels++
			continue
		}
		item := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: e.Text}
		if len(pending) > 0 {
			item.HeadComment = strings.Join(pending, "\n")
			pending = nil
		}
		seq.Content = append(seq.Content, item)
		urls++
	}

	root := &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "repos"},
			seq,
		},
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(doc)
	_ = enc.Close()
	return buf.Bytes(), urls, labels
}
