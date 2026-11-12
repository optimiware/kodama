package resource_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/resource"
)

func TestRegisterBindingValidatesMetadataBounds(t *testing.T) {
	entries := func(count int) map[string]string {
		metadata := make(map[string]string, count)
		for index := range count {
			metadata["key-"+string(rune('a'+index%26))+"-"+string(rune('a'+index/26))] = "value"
		}
		return metadata
	}
	tests := []struct {
		name     string
		metadata map[string]string
		want     fault.Class
	}{
		{name: "maximum entries", metadata: entries(resource.MaxMetadataEntries), want: ""},
		{name: "too many entries", metadata: entries(resource.MaxMetadataEntries + 1), want: fault.InvalidInput},
		{name: "maximum key length", metadata: map[string]string{strings.Repeat("k", resource.MaxMetadataKeyBytes): "value"}, want: ""},
		{name: "oversized key", metadata: map[string]string{strings.Repeat("k", resource.MaxMetadataKeyBytes+1): "value"}, want: fault.InvalidInput},
		{name: "maximum value length", metadata: map[string]string{"key": strings.Repeat("v", resource.MaxMetadataValueBytes)}, want: ""},
		{name: "oversized value", metadata: map[string]string{"key": strings.Repeat("v", resource.MaxMetadataValueBytes+1)}, want: fault.InvalidInput},
		{name: "empty key", metadata: map[string]string{"": "value"}, want: fault.InvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeBindingPorts{}
			calls := 0
			ports.apply = func(_ context.Context, unit resource.BindingUnit) (resource.BindingResult, error) {
				calls++
				return resource.BindingResult{Version: unit.Binding.Version}, nil
			}
			service := newBindingService(t, ports)
			command := validRegisterCommand()
			command.Metadata = test.metadata
			_, err := service.RegisterBinding(context.Background(), command)
			if test.want == "" {
				if err != nil {
					t.Fatalf("RegisterBinding: %v", err)
				}
				if calls != 1 {
					t.Fatalf("Apply calls = %d; want 1", calls)
				}
				return
			}
			requireFaultClass(t, err, test.want)
			if calls != 0 {
				t.Fatalf("Apply calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestValidateTrustedOrigins(t *testing.T) {
	valid := []string{
		"https://git.example.com",
		"https://git.example.com/shop",
		"https://git.example.com:8443/other",
		"https://git.example.com/shop/repo",
	}
	for _, origin := range valid {
		if err := resource.ValidateTrustedOrigins([]string{origin}); err != nil {
			t.Errorf("ValidateTrustedOrigins(%q) = %v; want accepted", origin, err)
		}
	}
	invalid := []string{
		"",
		"http://git.example.com/shop",
		"git.example.com/shop",
		"https://user@git.example.com/shop",
		"https://git.example.com/shop?ref=main",
		"https://git.example.com/shop#fragment",
		"https://git.example.com/../shop",
		"https://git.example.com/a/./shop",
		"https://git.example.com//shop",
		"https://git.example.com/%2e%2e/shop",
		"https://",
		"ftp://git.example.com/shop",
		"https://git.example.com/ shop",
	}
	for _, origin := range invalid {
		if err := resource.ValidateTrustedOrigins([]string{origin}); err == nil {
			t.Errorf("ValidateTrustedOrigins(%q) = nil; want refusal", origin)
		}
	}
	if err := resource.ValidateTrustedOrigins(nil); err != nil {
		t.Errorf("ValidateTrustedOrigins(nil) = %v; want accepted", err)
	}
}

func TestRegisterBindingUsesTrustedOriginValidation(t *testing.T) {
	ports := &fakeBindingPorts{}
	calls := 0
	ports.apply = func(context.Context, resource.BindingUnit) (resource.BindingResult, error) {
		calls++
		return resource.BindingResult{}, nil
	}
	service := newBindingService(t, ports)
	command := validRegisterCommand()
	command.TrustedOrigins = []string{"https://user@git.example.com/shop"}
	_, err := service.RegisterBinding(context.Background(), command)
	requireFaultClass(t, err, fault.InvalidInput)
	if calls != 0 {
		t.Fatalf("Apply calls = %d; want 0 before refusal", calls)
	}
}

func TestRegisterBindingRefusesNonLifecycleReportDeclarations(t *testing.T) {
	tests := []struct {
		name   string
		events []string
		want   fault.Class
	}{
		{name: "service declared report type", events: []string{"git.ref.updated"}, want: ""},
		{name: "lifecycle task event", events: []string{"task.created"}, want: fault.InvalidInput},
		{name: "lifecycle execution event", events: []string{"execution.queued"}, want: fault.InvalidInput},
		{name: "lifecycle verified report", events: []string{"resource.verified"}, want: fault.InvalidInput},
		{name: "lifecycle custom observation", events: []string{"observation.custom"}, want: fault.InvalidInput},
		{name: "empty report type", events: []string{""}, want: fault.InvalidInput},
		{name: "uppercase report type", events: []string{"Git.Ref.Updated"}, want: fault.InvalidInput},
		{name: "oversized report type", events: []string{strings.Repeat("a", 129)}, want: fault.InvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeBindingPorts{}
			calls := 0
			ports.apply = func(_ context.Context, unit resource.BindingUnit) (resource.BindingResult, error) {
				calls++
				return resource.BindingResult{Version: unit.Binding.Version}, nil
			}
			service := newBindingService(t, ports)
			command := validRegisterCommand()
			command.ReportableEvents = test.events
			_, err := service.RegisterBinding(context.Background(), command)
			if test.want == "" {
				if err != nil {
					t.Fatalf("RegisterBinding: %v", err)
				}
				if calls != 1 {
					t.Fatalf("Apply calls = %d; want 1", calls)
				}
				return
			}
			requireFaultClass(t, err, test.want)
			if calls != 0 {
				t.Fatalf("Apply calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestBindingUnitCarriesNoResourceBytesOrGitRules(t *testing.T) {
	forbidden := []string{"bytes", "content", "data", "rules", "accessrules", "provision", "gitrules"}
	for _, typ := range []reflect.Type{
		reflect.TypeFor[resource.Binding](),
		reflect.TypeFor[resource.BindingUnit](),
	} {
		for field := range typ.Fields() {
			// Metadata is the sanctioned bounded display/selector map; its exact
			// type is pinned below. The forbidden scan targets resource bytes,
			// content bodies and Git rules, and "metadata" necessarily contains
			// the "data" substring, so the sanctioned field is exempt.
			if field.Name == "Metadata" {
				continue
			}
			lower := strings.ToLower(field.Name)
			for _, name := range forbidden {
				if strings.Contains(lower, name) {
					t.Fatalf("%s carries forbidden field %q; bindings store metadata, never resource bytes or Git rules", typ, field.Name)
				}
			}
		}
	}
	metadata, ok := reflect.TypeFor[resource.Binding]().FieldByName("Metadata")
	if !ok || metadata.Type != reflect.TypeFor[map[string]string]() {
		t.Fatalf("Binding.Metadata type = %v; want map[string]string", metadata.Type)
	}
}

func TestOperationsVocabularyIsFrozenAndDefensive(t *testing.T) {
	operations := resource.Operations()
	want := []resource.Operation{resource.OperationGitRefUpdate, resource.OperationGitRead}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("Operations() = %v; want %v", operations, want)
	}
	operations[0] = resource.Operation("mutated")
	if again := resource.Operations(); !reflect.DeepEqual(again, want) {
		t.Fatalf("Operations() after caller mutation = %v; want the declared vocabulary", again)
	}
	if _, err := resource.ParseOperation("git.ref.update"); err != nil {
		t.Fatalf("ParseOperation(git.ref.update): %v", err)
	}
	if _, err := resource.ParseOperation("git.read"); err != nil {
		t.Fatalf("ParseOperation(git.read): %v", err)
	}
	if _, err := resource.ParseOperation("task.create"); err == nil {
		t.Fatal("ParseOperation(task.create) accepted a public operation as a resource operation")
	}
	if resource.Operation("").Valid() {
		t.Fatal("empty resource operation is valid")
	}
}
