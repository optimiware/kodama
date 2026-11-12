package registry_test

import (
	"context"
	"slices"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

// TestConfigValidate pins constructor-level refusal of unusable configuration.
func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name   string
		config registry.Config
	}{
		{"nil transport", registry.Config{}},
		{"negative response bound", registry.Config{Transport: &fakeTransport{}, MaxResponseBytes: -1}},
		{"negative redirect cap", registry.Config{Transport: &fakeTransport{}, MaxRedirects: -1}},
		{"empty media type", registry.Config{Transport: &fakeTransport{}, AllowedMediaTypes: []string{""}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			requireFault(t, testCase.config.Validate(), fault.InvalidInput)

			resolver, err := registry.New(testCase.config)
			requireFault(t, err, fault.InvalidInput)
			if resolver != nil {
				t.Fatalf("New returned a resolver for an invalid config")
			}
		})
	}

	if err := (registry.Config{Transport: &fakeTransport{}}).Validate(); err != nil {
		t.Fatalf("Validate refused a minimal valid config: %v", err)
	}
}

// TestNewAcceptsDefaults pins that zero bounds select the documented defaults
// instead of failing construction.
func TestNewAcceptsDefaults(t *testing.T) {
	resolver, err := registry.New(registry.Config{Transport: &fakeTransport{}})
	if err != nil {
		t.Fatalf("New refused the default config: %v", err)
	}
	if resolver == nil {
		t.Fatal("New returned a nil resolver for the default config")
	}
}

// TestDefaultAllowedMediaTypes pins the OCI/Docker manifest and index default
// allow-list and its copy semantics.
func TestDefaultAllowedMediaTypes(t *testing.T) {
	want := []string{
		dockerManifestMediaType,
		dockerIndexMediaType,
		ociManifestMediaType,
		ociIndexMediaType,
	}
	mediaTypes := registry.DefaultAllowedMediaTypes()
	if len(mediaTypes) != len(want) {
		t.Fatalf("DefaultAllowedMediaTypes() returned %d entries, want %d", len(mediaTypes), len(want))
	}
	for _, mediaType := range want {
		found := slices.Contains(mediaTypes, mediaType)
		if !found {
			t.Fatalf("DefaultAllowedMediaTypes() omitted %q: %v", mediaType, mediaTypes)
		}
	}

	mediaTypes[0] = "mutated"
	if registry.DefaultAllowedMediaTypes()[0] == "mutated" {
		t.Fatal("DefaultAllowedMediaTypes() returns a shared mutable slice")
	}
}

// TestResolveRequestValidate pins the declared request check the resolver must
// honor.
func TestResolveRequestValidate(t *testing.T) {
	requireFault(t, (pipeline.ResolveRequest{}).Validate(), fault.InvalidInput)

	if err := (pipeline.ResolveRequest{Reference: tagReference}).Validate(); err != nil {
		t.Fatalf("Validate refused a non-empty reference: %v", err)
	}
	if err := (pipeline.ResolveRequest{Reference: repository + "@" + childDigest}).Validate(); err != nil {
		t.Fatalf("Validate refused a pinned reference: %v", err)
	}
}

// TestResolveEmptyRequestRefused pins that Resolve refuses an empty request
// before any registry effect.
func TestResolveEmptyRequestRefused(t *testing.T) {
	transport := &fakeTransport{}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{})
	requireFault(t, err, fault.InvalidInput)
	requireNoDigest(t, image)
	if requests := transport.captured(); len(requests) != 0 {
		t.Fatalf("empty request still issued %d registry requests", len(requests))
	}
}
