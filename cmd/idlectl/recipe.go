package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/inerplat/idleloom/internal/recipe"
	"github.com/spf13/pflag"
	"sigs.k8s.io/yaml"
)

const recipeUsageText = `Usage:
  idlectl recipe list
  idlectl recipe show NAME@VERSION
  idlectl recipe render NAME@VERSION --name RUN [--values FILE] -o yaml
`

func runRecipe(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", strings.TrimSpace(recipeUsageText))
	}
	registry, err := recipe.DefaultRegistry()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		return runRecipeList(registry, args[1:], output)
	case "show":
		return runRecipeShow(registry, args[1:], output)
	case "render":
		return runRecipeRender(registry, args[1:], input, output)
	case "help", "-h", "--help":
		fmt.Fprint(output, recipeUsageText)
		return nil
	default:
		return fmt.Errorf("unknown recipe command %q\n%s", args[0], strings.TrimSpace(recipeUsageText))
	}
}

func runRecipeList(registry *recipe.Registry, args []string, output io.Writer) error {
	flags := recipeFlags("list", "idlectl recipe list", output)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return fmt.Errorf("usage: idlectl recipe list")
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "RECIPE\tBACKEND\tTASK\tRUNTIME\tDESCRIPTION")
	for _, definition := range registry.List() {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", definition.ID(), definition.Backend, definition.Task, definition.Runtime, definition.Description)
	}
	return writer.Flush()
}

func runRecipeShow(registry *recipe.Registry, args []string, output io.Writer) error {
	flags := recipeFlags("show", "idlectl recipe show NAME@VERSION", output)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 1 {
		return fmt.Errorf("usage: idlectl recipe show NAME@VERSION")
	}
	details, err := registry.Details(flags.Arg(0))
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(details)
	if err != nil {
		return err
	}
	_, err = output.Write(data)
	return err
}

func runRecipeRender(registry *recipe.Registry, args []string, input io.Reader, output io.Writer) error {
	flags := recipeFlags("render", "idlectl recipe render NAME@VERSION --name RUN [--values FILE] -o yaml", output)
	name := flags.String("name", "", "DNS label used for the rendered Kubernetes object and run identity")
	valuesPath := flags.StringP("values", "f", "", "YAML values file; use - for standard input")
	outputFormat := flags.StringP("output", "o", "yaml", "output format: yaml")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 1 || *name == "" {
		return fmt.Errorf("usage: idlectl recipe render NAME@VERSION --name RUN [--values FILE] -o yaml")
	}
	if *outputFormat != "yaml" {
		return fmt.Errorf("--output must be yaml")
	}
	values, err := readRecipeValues(*valuesPath, input)
	if err != nil {
		return err
	}
	result, err := registry.Render(flags.Arg(0), recipe.RenderOptions{Name: *name, Values: values})
	if err != nil {
		return err
	}
	_, err = output.Write(result.Manifest)
	return err
}

func readRecipeValues(path string, input io.Reader) ([]byte, error) {
	var (
		values []byte
		err    error
	)
	if path == "-" {
		values, err = io.ReadAll(input)
	} else if path != "" {
		values, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read recipe values: %w", err)
	}
	return values, nil
}

func recipeFlags(name, usage string, output io.Writer) *pflag.FlagSet {
	flags := pflag.NewFlagSet("recipe "+name, pflag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() { fmt.Fprintf(flags.Output(), "Usage:\n  %s\n", usage) }
	return flags
}
