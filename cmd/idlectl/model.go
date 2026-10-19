package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/devruntime"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const modelInspectUsage = "idlectl model inspect hf REPOSITORY@REVISION --name NAME [--max-context N] [--min-memory SIZE] [--endpoint URL]"

const modelUsageText = "Usage:\n  " + modelInspectUsage + `

Prints an IdleloomModel for an MLX safetensors repository pinned at one
commit, with the manifest digest and size computed from that commit's files.
`

func runModel(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return usagef("%s", strings.TrimSpace(modelUsageText))
	}
	switch args[0] {
	case "inspect":
		return runModelInspect(ctx, args[1:], output, &http.Client{Timeout: 5 * time.Minute})
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(output, modelUsageText)
		return nil
	default:
		return usagef("unknown model command %q\n%s", args[0], strings.TrimSpace(modelUsageText))
	}
}

func runModelInspect(ctx context.Context, args []string, output io.Writer, client *http.Client) error {
	flags := pflag.NewFlagSet("model inspect", pflag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() { _, _ = fmt.Fprintf(flags.Output(), "Usage:\n  %s\n", modelInspectUsage) }
	name := flags.String("name", "", "IdleloomModel name, a DNS subdomain")
	maxContext := flags.Int32("max-context", 4096, "maximum context length the model is admitted with")
	minMemory := flags.String("min-memory", "", "unified memory reservation; defaults to the conservative estimate for the size and context")
	endpoint := flags.String("endpoint", devruntime.DefaultHuggingFaceEndpoint, "Hugging Face endpoint")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 2 || flags.Arg(0) != "hf" || *name == "" {
		return usagef("usage: %s", modelInspectUsage)
	}
	if problems := validation.IsDNS1123Subdomain(*name); len(problems) > 0 {
		return usagef("--name %q: %s", *name, strings.Join(problems, "; "))
	}
	repository, revision, found := strings.Cut(flags.Arg(1), "@")
	if !found || repository == "" {
		return usagef("model source %q must be REPOSITORY@REVISION", flags.Arg(1))
	}
	source := nativev1alpha1.HuggingFaceSource{Repository: repository, Revision: revision}
	manifest, err := devruntime.ResolveHFManifest(ctx, client, *endpoint, repository, revision)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", flags.Arg(1), err)
	}
	memory := nativev1alpha1.MinimumUnifiedMemoryForModel(manifest.SizeBytes, *maxContext)
	if *minMemory != "" {
		memory, err = resource.ParseQuantity(*minMemory)
		if err != nil {
			return usagef("--min-memory %q: %v", *minMemory, err)
		}
	}
	model := nativev1alpha1.IdleloomModel{Spec: nativev1alpha1.IdleloomModelSpec{
		Family:         nativev1alpha1.ModelFamilyMLX,
		RuntimeProfile: nativev1alpha1.RuntimeProfileMLXLMV1,
		Artifact: nativev1alpha1.ModelArtifact{
			HuggingFace: &source, ManifestDigest: manifest.Digest,
			Format: nativev1alpha1.ArtifactFormatSafetensorsV1, SizeBytes: manifest.SizeBytes,
		},
		MinimumUnifiedMemory:  memory,
		MaxContextLength:      *maxContext,
		MaxConcurrentRequests: 1,
	}}
	model.Name = *name
	if err := nativev1alpha1.ValidateModel(&model); err != nil {
		return fmt.Errorf("the inspected model is not admissible: %w", err)
	}
	document := map[string]any{
		"apiVersion": nativev1alpha1.GroupVersion.String(),
		"kind":       "IdleloomModel",
		"metadata":   map[string]any{"name": model.Name},
		"spec":       model.Spec,
	}
	data, err := yaml.Marshal(document)
	if err != nil {
		return err
	}
	_, err = output.Write(data)
	return err
}
